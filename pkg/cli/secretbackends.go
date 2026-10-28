// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/secretbackend"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// WithSecretBackend adds a secret backend type (§20, #188): a backend of
// that type is configured in the secret_backends: section of
// --provider-config ({"secret_backends": {"<name>": {"type": "<type>", ...}}})
// and named by SecretRefs ("secretref:<name>/<path>").  A type registered
// twice — or the name of a built-in one ("vault-kv2", "command") — is a
// startup error.
func WithSecretBackend(typ string, f sdk.SecretBackendFactory) Option {
	return func(o *options) {
		o.secretTypes = append(o.secretTypes, namedSecretType{typ, f})
	}
}

type namedSecretType struct {
	typ string
	f   sdk.SecretBackendFactory
}

// secretTypes are the secret backend types of the run, set by Main.
var secretTypes map[string]sdk.SecretBackendFactory

func builtinSecretTypes() map[string]sdk.SecretBackendFactory {
	return map[string]sdk.SecretBackendFactory{
		secretbackend.VaultKV2Type: func(s map[string]string) (sdk.SecretBackend, error) { return secretbackend.NewVault(s) },
		secretbackend.CommandType:  func(s map[string]string) (sdk.SecretBackend, error) { return secretbackend.NewCommand(s) },
	}
}

func registerSecretTypes(extra []namedSecretType) error {
	types := builtinSecretTypes()
	for _, e := range extra {
		if e.typ == "" || e.f == nil {
			return fmt.Errorf("secret backend type: empty name or nil factory")
		}
		if _, dup := types[e.typ]; dup {
			return fmt.Errorf("secret backend type %q is registered twice", e.typ)
		}
		types[e.typ] = e.f
	}
	secretTypes = types
	return nil
}

// backendName is the shape of a secret backend name (the <backend> of a
// SecretRef).
var backendName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// loadSecretBackends builds the backends of the secret_backends: section of
// the --provider-config file at path (none when path is empty or the file
// has no such section).
func loadSecretBackends(path string, types map[string]sdk.SecretBackendFactory) (map[string]render.GenerationalSecretBackend, error) {
	if path == "" {
		return nil, nil
	}
	tree, err := loader.ReadTree(path)
	if err != nil {
		return nil, err
	}
	sec, ok := tree["secret_backends"]
	if !ok {
		return nil, nil
	}
	raw, ok := sec.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: \"secret_backends\" must be a map of backend name to settings", path)
	}
	if types == nil {
		types = builtinSecretTypes()
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]render.GenerationalSecretBackend, len(raw))
	for _, name := range names {
		if !backendName.MatchString(name) {
			return nil, fmt.Errorf("%s: secret_backends.%s: a backend name is lower-case letters, digits, - and _", path, name)
		}
		if name == "file" {
			return nil, fmt.Errorf("%s: secret_backends.file: \"file\" is the providers' file store (secret_store), not a configurable backend", path)
		}
		settings, ok := raw[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: secret_backends.%s must be a map of key to value", path, name)
		}
		m := make(map[string]string, len(settings))
		for k, v := range settings {
			switch x := v.(type) {
			case map[string]any, []any, nil:
				return nil, fmt.Errorf("%s: secret_backends.%s.%s must be a string, number or boolean", path, name, k)
			case string:
				m[k] = x
			case float64:
				m[k] = strconv.FormatFloat(x, 'f', -1, 64)
			default:
				m[k] = fmt.Sprint(x)
			}
		}
		typ := m["type"]
		delete(m, "type")
		f, ok := types[typ]
		if !ok {
			known := make([]string, 0, len(types))
			for t := range types {
				known = append(known, t)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("%s: secret_backends.%s: type %q is not a secret backend type (known: %v)", path, name, typ, known)
		}
		b, err := f(m)
		if err != nil {
			return nil, fmt.Errorf("%s: secret_backends.%s: %w", path, name, err)
		}
		out[name] = b
	}
	return out, nil
}

// openSecretBackends configures the secret backends of the --provider-config
// file for this process, and exits 1 when they cannot be used.
func openSecretBackends(commandName, path string) {
	backends, err := loadSecretBackends(path, secretTypes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: --provider-config: %v\n", commandName, err)
		os.Exit(1)
	}
	render.UseSecretBackends(backends)
}

// renderSecretGenerations is the secret-generation step of render phase 1
// (§20.2, #188) for a one-shot command: each unresolved ref is reported.
func renderSecretGenerations(commandName string, resources []diff.DesiredResource, logErr io.Writer) render.SecretGenResult {
	res := render.ResolveSecretGenerations(context.Background(), render.SecretBackends(), resources)
	for _, u := range res.Unresolved {
		fmt.Fprintf(logErr, "%s: %s: %s\n", commandName, u.ID, u.Reason())
	}
	return res
}
