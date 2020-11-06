// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	conntrack "github.com/mwitkow/go-conntrack"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/discovery/targetgroup"
)

const (
	watchTimeout = 2 * time.Minute

	componentName = "component_name"
	envLabel      = "env"
	// Constants for instrumentation.
	namespace = "prometheus"
)

var (
	rpcFailuresCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "sd_api_http_failures_total",
			Help:      "The number of Api http call failures.",
		})
	rpcDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  namespace,
			Name:       "sd_api_http_duration_seconds",
			Help:       "The duration of a Api http call in seconds.",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{"endpoint", "call"},
	)

	// DefaultSDConfig is the default Consul SD configuration.
	DefaultSDConfig = SDConfig{
		Scheme:          "http",
		Server:          "localhost:8500",
		AllowStale:      true,
		RefreshInterval: model.Duration(30 * time.Second),
	}
)

func init() {
	prometheus.MustRegister(rpcFailuresCount)
	prometheus.MustRegister(rpcDuration)
}

// SDConfig is the configuration for Consul service discovery.
type SDConfig struct {
	Server   string        `yaml:"server,omitempty"`
	Token    config.Secret `yaml:"token,omitempty"`
	Scheme   string        `yaml:"scheme,omitempty"`
	Username string        `yaml:"username,omitempty"`
	Password config.Secret `yaml:"password,omitempty"`

	TargetHost string `yaml:"host,omitempty"`

	// See https://www.consul.io/docs/internals/consensus.html#consistency-modes,
	// stale reads are a lot cheaper and are a necessity if you have >5k targets.
	AllowStale bool `yaml:"allow_stale"`
	// By default use blocking queries (https://www.consul.io/api/index.html#blocking-queries)
	// but allow users to throttle updates if necessary. This can be useful because of "bugs" like
	// https://github.com/hashicorp/consul/issues/3712 which cause an un-necessary
	// amount of requests on consul.
	RefreshInterval model.Duration `yaml:"refresh_interval,omitempty"`

	// See https://www.consul.io/api/catalog.html#list-services
	// The list of services for which targets are discovered.
	// Defaults to all services if empty.
	Services []string `yaml:"services,omitempty"`
	// A list of tags used to filter instances inside a service. Services must contain all tags in the list.
	ServiceTags []string `yaml:"tags,omitempty"`
	EnvTags     []string `yaml:"envs,omitempty"`
	// Desired node metadata.
	NodeMeta map[string]string `yaml:"node_meta,omitempty"`

	TLSConfig config.TLSConfig `yaml:"tls_config,omitempty"`

	ProxyURL config.URL `yaml:"proxy_url,omitempty"`

	Labels model.LabelSet `yaml:"labels"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.Server) == "" {
		return errors.New("api SD configuration requires a server address")
	}
	if c.TargetHost == "" {
		c.TargetHost = "dispositivos.bb.com.br"
	}
	return nil
}

// Discovery retrieves target information from a Consul server
// and updates them via watches.
type Discovery struct {
	conf            SDConfig
	refreshInterval time.Duration
	logger          log.Logger
	client          *http.Client
	token           string
}

// NewDiscovery returns a new Discovery for the given config.
func NewDiscovery(conf SDConfig, logger log.Logger) *Discovery {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	tls, err := config.NewTLSConfig(&conf.TLSConfig)
	if err != nil {
		return nil
	}
	transport := &http.Transport{
		IdleConnTimeout: 2 * time.Duration(watchTimeout),
		Proxy:           http.ProxyURL(conf.ProxyURL.URL),
		TLSClientConfig: tls,
		DialContext: conntrack.NewDialContextFunc(
			conntrack.DialWithTracing(),
			conntrack.DialWithName("api_sd"),
		),
	}
	wrapper := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(watchTimeout) + 15*time.Second,
	}

	cd := &Discovery{
		conf:            conf,
		client:          wrapper,
		refreshInterval: time.Duration(conf.RefreshInterval),
		logger:          logger,
	}
	return cd
}

// Run implements the Discoverer interface.
func (d *Discovery) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {

	// We need to watch the catalog.
	ticker := time.NewTicker(d.refreshInterval)

	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		default:
			tgroup := targetgroup.Group{
				Source:  "cmdweb",
				Labels:  model.LabelSet{},
				Targets: make([]model.LabelSet, 0),
			}
			for _, env := range d.conf.EnvTags {
				u := createURL(d.conf.Server, env, d.conf.ServiceTags)
				dresp, err := d.requestTargets(ctx, u, 100)
				if err != nil {
					continue
				}
				if dresp.Count > len(dresp.Results) {
					dresp, err = d.requestTargets(ctx, u, dresp.Count)
					if err != nil {
						continue
					}
				}
				for _, s := range dresp.Results {
					addr := fmt.Sprintf("%s.%s:%d", s.VirtualMachine.Name, d.conf.TargetHost, s.Port)
					labels := model.LabelSet{
						envLabel:           model.LabelValue(env),
						model.AddressLabel: model.LabelValue(addr),
						componentName:      model.LabelValue(s.CustomFields.ComponentName),
					}
					labels = labels.Merge(d.conf.Labels)
					tgroup.Targets = append(tgroup.Targets, labels)
				}
			}
			select {
			case <-ctx.Done():
			case ch <- []*targetgroup.Group{&tgroup}:
			}
			<-ticker.C
		}
	}
}

func (d *Discovery) requestTargets(ctx context.Context, u *url.URL, limit int) (*DjangoResponse, error) {
	q := u.Query()
	s := strconv.Itoa(limit)
	q.Set("limit", s)
	u.RawQuery = q.Encode()
	level.Info(d.logger).Log("apiUrl", u.String())
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		level.Error(d.logger).Log("msg", "Error retrieving api endpoint", "err", err)
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Token %s", d.conf.Token))
	resp, err := d.client.Do(req.WithContext(ctx))
	if err != nil {
		level.Error(d.logger).Log("msg", "Error retrieving api endpoint", "err", err)
		return nil, err
	}
	defer resp.Body.Close()
	dresp := new(DjangoResponse)
	err = json.NewDecoder(resp.Body).Decode(dresp)
	if err != nil {
		level.Error(d.logger).Log("msg", "Error decoding response from api endpoint", "err", err)
		return nil, err
	}
	level.Debug(d.logger).Log("msg", "Targets found", "targets", fmt.Sprintf("%d", dresp.Count))
	return dresp, nil
}

func createURL(server string, env string, tags []string) *url.URL {
	u, err := url.Parse(server)
	if err != nil {
		fmt.Print("Cannot parse url")
		return nil
	}
	q := u.Query()
	q.Add("tag", env)
	for _, t := range tags {
		q.Add("tag", t)
	}
	u.RawQuery = q.Encode()
	return u
}

type Service struct {
	ID             int         `json:"id"`
	Device         interface{} `json:"device"`
	VirtualMachine struct {
		ID   int    `json:"id"`
		URL  string `json:"url"`
		Name string `json:"name"`
	} `json:"virtual_machine"`
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Protocol struct {
		Value string `json:"value"`
		Label string `json:"label"`
		ID    int    `json:"id"`
	} `json:"protocol"`
	Ipaddresses  []interface{} `json:"ipaddresses"`
	Description  string        `json:"description"`
	Tags         []string      `json:"tags"`
	CustomFields struct {
		ComponentName    string      `json:"component_name"`
		ComponentVersion interface{} `json:"component_version"`
		ConsoleURL       string      `json:"console_url"`
		ContextName      string      `json:"context_name"`
		Serverstart      interface{} `json:"serverstart"`
		WlsVersion       string      `json:"wls_version"`
	} `json:"custom_fields"`
	Created     string `json:"created"`
	LastUpdated string `json:"last_updated"`
}

type DjangoResponse struct {
	Count    int
	Next     string
	Previous string
	Results  []Service
}
