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
	"github.com/tjst-t/sakutei/internal/dnszone"
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
//   - "dnszone": RFC1035 zone-file provider (dnszone.New, S62dbba-3).
//     config["zone"] is the path to the zone file.  If absent, defaults to
//     "sakutei-zone.db".  Manages A/AAAA/CNAME/TXT/MX/… records; SOA and NS
//     are excluded (management boundary, decisions.json S62dbba).
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

	// "dnszone" — RFC1035 zone-file provider (S62dbba-3).
	// config["zone"] is the path to the zone file.
	// config["state"] is accepted as an alias for "zone" so that the common
	// --state flag can be used for both providers without extra CLI plumbing.
	r.Register("dnszone", func(config map[string]string) (provider.Provider, error) {
		zonePath := config["zone"]
		if zonePath == "" {
			// Accept --state as an alias (CLI passes --state for all providers).
			zonePath = config["state"]
		}
		if zonePath == "" {
			zonePath = "sakutei-zone.db"
		}
		return dnszone.New(zonePath), nil
	})

	return r
}
