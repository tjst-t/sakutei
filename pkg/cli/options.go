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
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
)

// Option configures Main.
type Option func(*options)

type options struct {
	extra []*registry.ProviderRegistry
	// realizers are the network_policy realizers WithRealizer adds (#174).
	realizers []namedRealizer
	// secretTypes are the secret backend types WithSecretBackend adds (#188).
	secretTypes []namedSecretType
	// confirmChecks are the check functions WithConfirmCheck adds (#200).
	confirmChecks []namedConfirmCheck
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
			if spec, ok := e.IntentRangesOf(name); ok {
				reg.RegisterIntentRanges(name, spec)
			}
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

	// intent is the loaded intent's providers: section (§7.7, #181), set by
	// useIntent; Get merges a provider's entry into its config.  Before
	// useIntent (a command that reads no intent) the config is used as is.
	intent        map[string]map[string]any
	useIntentDone bool
	warnTo        io.Writer
	warnName      string
	warned        map[string]bool

	// access is the run's provider.ConfigAccess (§20.7, #185): set to
	// provider.AccessObserve by observeOnly for a run that never writes, so
	// the providers neither check nor take out their write credentials.
	access string
}

// observeOnly marks the run as one that never writes (plan, status, a
// reconcile with --mode alert-only or --freeze, ...): every provider is built
// with provider.ConfigAccess = provider.AccessObserve (§20.7, #185).
func (s *providerSet) observeOnly() *providerSet {
	s.access = provider.AccessObserve
	return s
}

// useIntent makes Get take the range settings of the intent's providers:
// section (§7.7, #181) and write the warnings (a setting only in the
// provider config) to w, each once.  An entry naming an unknown provider, or
// one that takes no range settings, is an error.
func (s *providerSet) useIntent(entries map[string]map[string]any, commandName string, w io.Writer) error {
	if err := s.reg.CheckIntentProviders(entries); err != nil {
		return fmt.Errorf("%s: %w", commandName, err)
	}
	s.intent, s.useIntentDone, s.warnTo, s.warnName = entries, true, w, commandName
	return nil
}

// Lookup reports whether name is registered.
func (s *providerSet) Lookup(name string) (registry.ProviderFactory, bool) {
	return s.reg.Lookup(name)
}

// Get builds the named provider from base overlaid with its
// --provider-config values.  base is not modified.
func (s *providerSet) Get(name string, base map[string]string) (provider.Provider, error) {
	if !s.useIntentDone {
		return s.reg.Get(name, s.configFor(name, base))
	}
	p, warnings, err := s.reg.GetWithIntent(name, s.configFor(name, base), s.intent[name])
	if err != nil {
		return nil, err
	}
	s.warn(warnings)
	return p, nil
}

func (s *providerSet) warn(warnings []string) {
	for _, w := range warnings {
		if s.warned[w] || s.warnTo == nil {
			continue
		}
		if s.warned == nil {
			s.warned = map[string]bool{}
		}
		s.warned[w] = true
		fmt.Fprintf(s.warnTo, "%s: warn: %s\n", s.warnName, w)
	}
}

// configured reports whether the run sets up the named provider: it has a
// section in --provider-config, or one of its own flags (configKeysOf) is
// set in base (#164).
func (s *providerSet) configured(name string, base map[string]string) bool {
	if _, ok := s.overrides[name]; ok {
		return true
	}
	for _, k := range configKeysOf[name] {
		if base[k] != "" {
			return true
		}
	}
	return false
}

func (s *providerSet) configFor(name string, base map[string]string) map[string]string {
	over := s.overrides[name]
	if len(over) == 0 && s.access == "" {
		return base
	}
	cfg := make(map[string]string, len(base)+len(over)+1)
	for k, v := range base {
		cfg[k] = v
	}
	for k, v := range over {
		cfg[k] = v
	}
	if s.access != "" {
		cfg[provider.ConfigAccess] = s.access
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
		if k != "providers" && k != "secret_backends" {
			return nil, fmt.Errorf("%s: unknown key %q (want \"providers\" and \"secret_backends\")", path, k)
		}
	}
	if _, ok := tree["providers"]; !ok {
		// Only secret_backends: (#188), read by openSecretBackends.
		return map[string]map[string]string{}, nil
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
			if k == provider.ConfigAccess {
				// §20.7 (#185): whether the run writes follows from the
				// command and its mode, never from the file.
				return nil, fmt.Errorf("%s: providers.%s.%s: set by the run (observe for plan, status and --mode alert-only / --freeze), not in --provider-config", path, name, k)
			}
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

// intentResolver is the daemon's reconcile.IntentProviderResolver (§7.7,
// #181): the providers of the run, each built with the intent's
// providers.<name>: entry of the cycle.
type intentResolver struct {
	set  *providerSet
	base map[string]string
}

func (r intentResolver) CheckIntentProviders(entries map[string]map[string]any) error {
	return r.set.reg.CheckIntentProviders(entries)
}

func (r intentResolver) ResolveWithIntent(name string, entry map[string]any) (provider.Provider, []string, error) {
	return r.set.reg.GetWithIntent(name, r.set.configFor(name, r.base), entry)
}
