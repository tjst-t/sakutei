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

package provider

import (
	"strings"
	"testing"
)

// flatRanges keeps the setting "scope" in config["s"].
type flatRanges struct{}

func (flatRanges) FromIntent(entry map[string]any, _ map[string]string) ([]RangeSetting, error) {
	if err := IntentKeys(entry, "scope"); err != nil {
		return nil, err
	}
	v, ok := entry["scope"]
	if !ok {
		return nil, nil
	}
	sc, err := IntentScope(v)
	if err != nil {
		return nil, err
	}
	return []RangeSetting{{Key: "scope", Setting: "s", Value: sc.String()}}, nil
}

func (flatRanges) FromConfig(config map[string]string) ([]RangeSetting, error) {
	if config["s"] == "" {
		return nil, nil
	}
	return []RangeSetting{{Key: "scope", Setting: "s", Value: config["s"]}}, nil
}

func (flatRanges) Apply(config map[string]string, take []RangeSetting) error {
	for _, t := range take {
		config["s"] = t.Value
	}
	return nil
}

func TestMergeRanges(t *testing.T) {
	tokyo := map[string]any{"scope": map[string]any{"site": "tokyo"}}
	cases := map[string]struct {
		entry   map[string]any
		config  map[string]string
		want    string
		warn    bool
		errWant string
	}{
		"none":                               {nil, map[string]string{}, "", false, ""},
		"intent only":                        {tokyo, map[string]string{}, "site=tokyo", false, ""},
		"config only":                        {nil, map[string]string{"s": "site=tokyo"}, "site=tokyo", true, ""},
		"config only, entry without the key": {map[string]any{}, map[string]string{"s": "site=tokyo"}, "site=tokyo", true, ""},
		"both agree":                         {tokyo, map[string]string{"s": "site=tokyo"}, "site=tokyo", false, ""},
		"both disagree":                      {tokyo, map[string]string{"s": "site=osaka"}, "", false, "disagree"},
		"unknown key":                        {map[string]any{"zone_scope": "x"}, map[string]string{}, "", false, "unknown key"},
	}
	for name, c := range cases {
		got, warnings, err := MergeRanges("p", flatRanges{}, c.entry, c.config)
		if c.errWant != "" {
			if err == nil || !strings.Contains(err.Error(), c.errWant) {
				t.Errorf("%s: err = %v, want %q", name, err, c.errWant)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got["s"] != c.want || (len(warnings) > 0) != c.warn {
			t.Errorf("%s: s=%q warnings=%v, want s=%q warn=%v", name, got["s"], warnings, c.want, c.warn)
		}
	}
	// The caller's config is not modified.
	cfg := map[string]string{}
	if _, _, err := MergeRanges("p", flatRanges{}, tokyo, cfg); err != nil || len(cfg) != 0 {
		t.Fatalf("MergeRanges modified the config: %v %v", cfg, err)
	}
}
