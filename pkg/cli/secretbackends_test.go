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
	"os"
	"path/filepath"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/registry"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

type constBackend struct{ settings map[string]string }

func (constBackend) Generation(context.Context, string) (int64, error)      { return 1, nil }
func (constBackend) Resolve(context.Context, string, int64) (string, error) { return "", nil }

func TestWithSecretBackend(t *testing.T) {
	defer registerSecretTypes(nil)
	f := func(s map[string]string) (sdk.SecretBackend, error) { return constBackend{s}, nil }
	var o options
	WithSecretBackend("mine", f)(&o)
	if err := registerSecretTypes(o.secretTypes); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pc.yaml")
	if err := os.WriteFile(path, []byte("secret_backends:\n  s1:\n    type: mine\n    k: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSecretBackends(path, secretTypes)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := got["s1"].(constBackend); !ok || b.settings["k"] != "3" || len(b.settings) != 1 {
		t.Errorf("backends = %#v", got)
	}
	// A secret_backends-only file is a valid --provider-config.
	if over, err := loadProviderConfig(path, registry.DefaultRegistry()); err != nil || len(over) != 0 {
		t.Errorf("loadProviderConfig = %v, %v", over, err)
	}
	for _, dup := range []string{"mine", "vault-kv2"} {
		WithSecretBackend(dup, f)(&o)
		if err := registerSecretTypes(o.secretTypes); err == nil {
			t.Errorf("type %q registered twice: want an error", dup)
		}
		o.secretTypes = o.secretTypes[:1]
	}
}

func TestLoadSecretBackends_Errors(t *testing.T) {
	for name, body := range map[string]string{
		"not a map":    `{"secret_backends": []}`,
		"entry":        `{"secret_backends": {"v": "x"}}`,
		"no type":      `{"secret_backends": {"v": {}}}`,
		"unknown type": `{"secret_backends": {"v": {"type": "nope"}}}`,
		"file":         `{"secret_backends": {"file": {"type": "command", "command": "/bin/sh"}}}`,
		"bad name":     `{"secret_backends": {"V/x": {"type": "command", "command": "/bin/sh"}}}`,
		"nested":       `{"secret_backends": {"v": {"type": "command", "command": {"a": 1}}}}`,
		"factory":      `{"secret_backends": {"v": {"type": "vault-kv2"}}}`,
	} {
		path := filepath.Join(t.TempDir(), "pc.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSecretBackends(path, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if got, err := loadSecretBackends("", nil); err != nil || got != nil {
		t.Errorf("no file: %v %v", got, err)
	}
}
