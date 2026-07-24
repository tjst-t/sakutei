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

package registry

import (
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// DefaultRegistry returns a new ProviderRegistry pre-loaded with the built-in
// providers:
//
//   - "fake": in-memory, file-backed fake provider (fakeprovider.New).
//     config["state"] is the path to the state file.  Defaults to
//     "sakutei-fake-state.json" when the key is absent.
//
// S62dbba-3: the "dnszone" provider will be registered here when that Story
// lands.  The registration site (this function) is the only place that knows
// about concrete provider packages; cmd/ depends only on the registry seam.
func DefaultRegistry() *ProviderRegistry {
	r := New()

	// "fake" — file-backed fake provider.
	// config["state"] is the path to the JSON state file.
	r.Register("fake", func(config map[string]string) (provider.Provider, error) {
		statePath := config["state"]
		if statePath == "" {
			statePath = "sakutei-fake-state.json"
		}
		return fakeprovider.New(statePath), nil
	})

	return r
}
