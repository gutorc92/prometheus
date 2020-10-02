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

package consul

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	conntrack "github.com/mwitkow/go-conntrack"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/discovery"
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
	discovery.RegisterConfig(&SDConfig{})
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
}

// Name returns the name of the Config.
func (*SDConfig) Name() string { return "api" }

// NewDiscoverer returns a Discoverer for the Config.
func (c *SDConfig) NewDiscoverer(opts discovery.DiscovererOptions) (discovery.Discoverer, error) {
	return NewDiscovery(c, opts.Logger)
}

// SetDirectory joins any relative file paths with dir.
func (c *SDConfig) SetDirectory(dir string) {
	c.TLSConfig.SetDirectory(dir)
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
		return errors.New("consul SD configuration requires a server address")
	}
	return nil
}

// Discovery retrieves target information from a Consul server
// and updates them via watches.
type Discovery struct {
	refreshInterval time.Duration
	logger          log.Logger
	client          *http.Client
	token           string
	envTags         []string
	tags            []string
	server          string
}

// NewDiscovery returns a new Discovery for the given config.
func NewDiscovery(conf *SDConfig, logger log.Logger) (*Discovery, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	tls, err := config.NewTLSConfig(&conf.TLSConfig)
	if err != nil {
		return nil, err
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
		client:          wrapper,
		refreshInterval: time.Duration(conf.RefreshInterval),
		logger:          logger,
		envTags:         conf.EnvTags,
		tags:            conf.ServiceTags,
		token:           fmt.Sprintf("Token %s", conf.Token),
		server:          conf.Server,
	}
	return cd, nil
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
			for _, env := range d.envTags {
				u := createURL(d.server, env, d.tags)
				level.Info(d.logger).Log("url", u.String())
				req, err := http.NewRequest("GET", u.String(), nil)
				req.Header.Set("Authorization", d.token)
				if err != nil {
					fmt.Print(err)
				}
				resp, err := d.client.Do(req.WithContext(ctx))
				if err != nil {
					level.Error(d.logger).Log("msg", "Error retrieving api endpoint", "err", err)
					fmt.Print(err)
				}
				if resp.StatusCode != http.StatusOK {
					fmt.Println("Error on get response", resp.StatusCode)
				}
				defer resp.Body.Close()
				dresp := new(DjangoResponse)
				err = json.NewDecoder(resp.Body).Decode(dresp)
				if err != nil {
					level.Error(d.logger).Log("msg", "Error decoding response from api endpoint", "err", err)
				}
				fmt.Println("Quantidade de resultados", dresp.Count)
				tgroup := targetgroup.Group{
					Source: u.String(),
					Labels: model.LabelSet{
						envLabel: model.LabelValue(env),
					},
					Targets: make([]model.LabelSet, 0, dresp.Count),
				}
				for _, s := range dresp.Results {
					addr := fmt.Sprintf("%s.dispositivos.bb.com.br:%d", s.VirtualMachine.Name, s.Port)
					labels := model.LabelSet{
						model.AddressLabel: model.LabelValue(addr),
						componentName:      model.LabelValue(s.CustomFields.ComponentName),
					}
					tgroup.Targets = append(tgroup.Targets, labels)
				}
				select {
				case <-ctx.Done():
				case ch <- []*targetgroup.Group{&tgroup}:
				}
			}
			fmt.Print("D")
			<-ticker.C
		}
	}
}

func createURL(server string, env string, tags []string) *url.URL {
	u, err := url.Parse(server)
	if err != nil {
		fmt.Print("Cannot parse url")
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
