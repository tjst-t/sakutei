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

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
)

func TestBuildRegistry_RejectsDuplicates(t *testing.T) {
	f := func(map[string]string) (provider.Provider, error) { return nil, nil }
	mine := registry.New()
	mine.Register("mine", f)
	reg, err := buildRegistry([]*registry.ProviderRegistry{mine})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Lookup("mine"); !ok {
		t.Error("mine is not registered")
	}
	if _, ok := reg.Lookup("fake"); !ok {
		t.Error("the built-in providers are gone")
	}
	dup := registry.New()
	dup.Register("fake", f)
	if _, err := buildRegistry([]*registry.ProviderRegistry{dup}); err == nil || !strings.Contains(err.Error(), `"fake"`) {
		t.Errorf("a built-in name: err = %v", err)
	}
	if _, err := buildRegistry([]*registry.ProviderRegistry{mine, mine}); err == nil {
		t.Error("the same name from two registries: want an error")
	}
}

func TestProviderConfig_OverlaysBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pc.json")
	body := `{"providers": {"fake": {"state": "file.json", "n": 3, "on": true}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.DefaultRegistry()
	over, err := loadProviderConfig(path, reg)
	if err != nil {
		t.Fatal(err)
	}
	s := &providerSet{reg: reg, overrides: over}
	base := map[string]string{"state": "flag.json", "zone": "z.db"}
	got := s.configFor("fake", base)
	if got["state"] != "file.json" || got["zone"] != "z.db" || got["n"] != "3" || got["on"] != "true" {
		t.Errorf("fake config = %v", got)
	}
	if base["state"] != "flag.json" {
		t.Error("the base config was modified")
	}
	if got := s.configFor("dnszone", base); got["state"] != "flag.json" {
		t.Errorf("dnszone config = %v, want the base", got)
	}
}

func TestProviderConfig_Errors(t *testing.T) {
	for name, body := range map[string]string{
		"unknown provider": `{"providers": {"nope": {}}}`,
		"unknown key":      `{"providers": {}, "extra": 1}`,
		"not a map":        `{"providers": {"fake": "x"}}`,
		"nested value":     `{"providers": {"fake": {"state": {"a": "b"}}}}`,
	} {
		path := filepath.Join(t.TempDir(), "pc.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadProviderConfig(path, registry.DefaultRegistry()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
