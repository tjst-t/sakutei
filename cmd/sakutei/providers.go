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

package main

// providers.go — multi-provider resolution helpers for cmd/sakutei.
//
// resolveProviders collects the unique set of provider names declared in
// the loaded intent resources, looks each up in the ProviderRegistry, and
// returns a name→Provider map ready for routing.
//
// Design rationale (S62dbba-2):
//   - A single intent directory may declare resources whose "provider" field
//     names differ (e.g. "fake" and "dnszone").
//   - The one-shot plan/reconcile commands need one provider.Provider instance
//     per name so they can route fp.Read and fp.Apply to the correct backend.
//   - The registry is the single place that knows about concrete provider
//     packages; this file depends only on the pkg/registry seam.

import (
	"fmt"
	"os"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// resolveProviders builds a name→provider.Provider map for all provider names
// referenced in the given desired resources.
//
// reg is the registry to look up factories in.
// baseConfig is the per-invocation configuration passed to every factory
// (e.g. {"state": "/tmp/state.json"} for the fake provider).
//
// Returns an error if any provider name is not registered or if a factory call
// fails.  Also runs sdk.ValidateProvider on every resolved provider (fail-closed,
// §10.4, ADR-0008) and exits 1 via exitOnValidationFail when validation fails.
func resolveProviders(
	reg *registry.ProviderRegistry,
	resources []diff.DesiredResource,
	baseConfig map[string]string,
	commandName string,
) (map[string]provider.Provider, error) {
	// Collect unique provider names from the loaded resources.
	names := make(map[string]struct{})
	for _, r := range resources {
		names[r.ID.Provider] = struct{}{}
	}

	result := make(map[string]provider.Provider, len(names))
	for name := range names {
		// Build a per-provider config: start from baseConfig and override with
		// provider-specific keys if needed.  For now all providers share the
		// same baseConfig (the factory picks what it needs from it).
		p, err := reg.Get(name, baseConfig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", commandName, err)
		}
		// Fail-closed provider validation (§10.4, ADR-0008).
		if err := sdk.ValidateProvider(p); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: provider validation failed (%q): %v\n", name, err)
			os.Exit(1)
		}
		result[name] = p
	}
	return result, nil
}
