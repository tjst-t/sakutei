// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command sakutei-kvfile is the sakutei CLI with one provider of its own
// added (§28.9): "kvfile", which keeps key/value records in a JSON file.
// Every built-in provider and every verb, flag and exit code stay as they
// are; kvfile reads its settings from --provider-config:
//
//	{"providers": {"kvfile": {"path": "records.json"}}}
package main

import (
	"example.com/sakutei-external-provider/kvfile"

	"github.com/tjst-t/sakutei/pkg/cli"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
)

func main() {
	cli.Main(cli.WithProviders(func(r *registry.ProviderRegistry) {
		r.Register(kvfile.Name, func(config map[string]string) (provider.Provider, error) {
			return kvfile.New(config)
		})
	}))
}
