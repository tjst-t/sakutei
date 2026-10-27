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

package cli

// options.go — the seam through which another module adds its own providers
// (#119, §28.9): Main(WithProviders(...)) builds a binary that is the sakutei
// CLI plus those providers, and --provider-config hands each provider its own
// settings on top of the flag-derived base config.

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
)

// Option configures Main.
type Option func(*options)

type options struct {
	extra []*registry.ProviderRegistry
}

// WithRegistry adds every provider registered in reg to the built-in ones
// (registry.DefaultRegistry).  A name that is already registered — built in,
// or by an earlier option — is a startup error: nothing is overwritten.
func WithRegistry(reg *registry.ProviderRegistry) Option {
	return func(o *options) { o.extra = append(o.extra, reg) }
}

// WithProviders calls register with an empty registry and adds what it
// registers, with the same rule as WithRegistry.
func WithProviders(register func(*registry.ProviderRegistry)) Option {
	return func(o *options) {
		reg := registry.New()
		register(reg)
		o.extra = append(o.extra, reg)
	}
}

// baseRegistry is the registry every command resolves providers from.  Main
// sets it before dispatching; it is DefaultRegistry plus the options' ones.
var baseRegistry *registry.ProviderRegistry

// buildRegistry returns DefaultRegistry with the providers of extra added,
// or an error naming the first provider registered twice.
func buildRegistry(extra []*registry.ProviderRegistry) (*registry.ProviderRegistry, error) {
	reg := registry.DefaultRegistry()
	for _, e := range extra {
		if e == nil {
			continue
		}
		for _, name := range e.Names() {
			if _, dup := reg.Lookup(name); dup {
				return nil, fmt.Errorf("provider %q is registered twice (an added provider must not reuse a registered name)", name)
			}
			f, _ := e.Lookup(name)
			reg.Register(name, f)
		}
	}
	return reg, nil
}

// providerSet is the registry as one command sees it: Get lays the
// --provider-config values of the named provider over the base config built
// from the flags (the file's values win).
type providerSet struct {
	reg       *registry.ProviderRegistry
	overrides map[string]map[string]string
}

// Lookup reports whether name is registered.
func (s *providerSet) Lookup(name string) (registry.ProviderFactory, bool) {
	return s.reg.Lookup(name)
}

// Get builds the named provider from base overlaid with its
// --provider-config values.  base is not modified.
func (s *providerSet) Get(name string, base map[string]string) (provider.Provider, error) {
	return s.reg.Get(name, s.configFor(name, base))
}

func (s *providerSet) configFor(name string, base map[string]string) map[string]string {
	over := s.overrides[name]
	if len(over) == 0 {
		return base
	}
	cfg := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		cfg[k] = v
	}
	for k, v := range over {
		cfg[k] = v
	}
	return cfg
}

// providerConfigFlag registers --provider-config on a command that builds
// providers.
func providerConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("provider-config", "",
		`per-provider settings (JSON / YAML): {"providers": {"<name>": {"<key>": "<value>", ...}}}; laid over the config built from the other flags (the file's values win, §28.9)`)
}

// openProviderSet returns the providers of this run with the --provider-config
// file (empty: none) applied, and exits 1 when the file cannot be used.
func openProviderSet(commandName, path string) *providerSet {
	reg := baseRegistry
	if reg == nil {
		reg = registry.DefaultRegistry()
	}
	set := &providerSet{reg: reg}
	if path == "" {
		return set
	}
	over, err := loadProviderConfig(path, reg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: --provider-config: %v\n", commandName, err)
		os.Exit(1)
	}
	set.overrides = over
	return set
}

// loadProviderConfig reads a --provider-config file.  Every provider named in
// it must be registered (a typo is not silently ignored); values must be
// scalars, and are passed to the factory as strings.
func loadProviderConfig(path string, reg *registry.ProviderRegistry) (map[string]map[string]string, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return nil, err
	}
	for k := range tree {
		if k != "providers" {
			return nil, fmt.Errorf("%s: unknown key %q (want only \"providers\")", path, k)
		}
	}
	raw, ok := tree["providers"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: \"providers\" must be a map of provider name to settings", path)
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]map[string]string, len(raw))
	for _, name := range names {
		if _, known := reg.Lookup(name); !known {
			return nil, fmt.Errorf("%s: unknown provider: %s", path, name)
		}
		settings, ok := raw[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: providers.%s must be a map of key to value", path, name)
		}
		m := make(map[string]string, len(settings))
		for k, v := range settings {
			switch x := v.(type) {
			case map[string]any, []any, nil:
				return nil, fmt.Errorf("%s: providers.%s.%s must be a string, number or boolean", path, name, k)
			case string:
				m[k] = x
			case float64:
				m[k] = strconv.FormatFloat(x, 'f', -1, 64)
			default:
				m[k] = fmt.Sprint(x)
			}
		}
		out[name] = m
	}
	return out, nil
}
