// Copyright 2024 Redpanda Data, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package redis

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/redpanda-data/benthos/v4/public/service"
)

func clientFields() []*service.ConfigField {
	tlsField := service.NewTLSToggledField("tls").
		Description(`Custom TLS settings can be used to override system defaults.

**Troubleshooting**

Some cloud hosted instances of Redis (such as Azure Cache) might need some hand holding in order to establish stable connections. Unfortunately, it is often the case that TLS issues will manifest as generic error messages such as "i/o timeout". If you're using TLS and are seeing connectivity problems consider setting ` + "`enable_renegotiation` to `true`" + `, and ensuring that the server supports at least TLS version 1.2.`)

	return []*service.ConfigField{
		service.NewURLField("url").
			Description("The URL of the target Redis server. Database is optional and is supplied as the URL path.").
			Example("redis://:6379").
			Example("redis://localhost:6379").
			Example("redis://foousername:foopassword@redisplace:6379").
			Example("redis://:foopassword@redisplace:6379").
			Example("redis://localhost:6379/1").
			Example("redis://localhost:6379/1,redis://localhost:6380/1"),
		service.NewStringEnumField("kind", "simple", "cluster", "failover").
			Description("Specifies a simple, cluster-aware, or failover-aware redis client.").
			Default("simple").
			Advanced(),
		service.NewStringField("master").
			Description("Name of the redis master when `kind` is `failover`").
			Default("").
			Example("mymaster").
			Advanced(),
		tlsField,
	}
}

func getClient(parsedConf *service.ParsedConfig) (redis.UniversalClient, error) {
	urlStr, err := parsedConf.FieldString("url")
	if err != nil {
		return nil, err
	}

	kind, err := parsedConf.FieldString("kind")
	if err != nil {
		return nil, err
	}

	master, err := parsedConf.FieldString("master")
	if err != nil {
		return nil, err
	}

	tlsConf, tlsEnabled, err := parsedConf.FieldTLSToggled("tls")
	if err != nil {
		return nil, err
	}
	if !tlsEnabled {
		tlsConf = nil
	}

	// We default to Redis DB 0 for backward compatibility
	var redisDB int
	var user string
	var pass string
	var addrs []string

	// handle comma-separated urls
	for v := range strings.SplitSeq(urlStr, ",") {
		url, err := url.Parse(v)
		if err != nil {
			return nil, err
		}

		if url.Scheme == "tcp" {
			url.Scheme = "redis"
		}

		rurl, err := redis.ParseURL(url.String())
		if err != nil {
			return nil, err
		}

		addrs = append(addrs, rurl.Addr)
		redisDB = rurl.DB
		user = rurl.Username
		pass = rurl.Password
	}

	var client redis.UniversalClient
	opts := &redis.UniversalOptions{
		Addrs:     addrs,
		DB:        redisDB,
		Username:  user,
		Password:  pass,
		TLSConfig: tlsConf,
	}

	switch kind {
	case "simple":
		client = redis.NewClient(opts.Simple())
	case "cluster":
		client = redis.NewClusterClient(opts.Cluster())
	case "failover":
		opts.MasterName = master
		client = redis.NewFailoverClient(opts.Failover())
	default:
		err = fmt.Errorf("invalid redis kind: %s", kind)
	}

	return client, err
}
