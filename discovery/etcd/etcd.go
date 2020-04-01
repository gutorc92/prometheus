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
package etcd

import (
	"context"
	"flag"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/coreos/etcd/clientv3"
)

var (
	// DefaultSDConfig is the default Etcd SD configuration.
	DefaultSDConfig = SDConfig{
		etcdBase:           "services"
		etcdServiceName:    "prometheus"
	}
)

type SDConfig struct {
	Server       string
}

// Discovery retrieves target information from a Consul server
// and updates them via watches.
type Discovery struct {
	client           *clientv3.Client
	servicePath      string
	clientDatacenter string
	tagSeparator     string
	watchedServices  []string // Set of services which will be discovered.
	watchedTags      []string // Tags used to filter instances of a service.
	watchedNodeMeta  map[string]string
	allowStale       bool
	refreshInterval  time.Duration
	finalizer        func()
	logger           log.Logger
}

// NewDiscovery returns a new Discovery for the given config.
func NewDiscovery(conf *SDConfig, logger log.Logger) (*Discovery, error) {
	servicePath := fmt.Sprintf("%s/%s/", conf.etcdBase, conf.etcdServiceName)
	cliScrape, err := clientv3.New(clientv3.Config{Endpoints: endpointsScrape, DialTimeout: 10 * time.Second})
	cd := Discovery{
		client: cliScrape,
		servicePath: servicePath
	}
	return cd, nil
}