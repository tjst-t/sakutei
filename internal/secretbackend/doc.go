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

// Package secretbackend holds the built-in secret backends a run can
// configure besides the file store (§20, #188): Vault KV v2 (tested against
// OpenBao) and an outside command.  Each one gives the generation of a secret
// (metadata only) for render phase 1 and its value at Apply time.  A value is
// never put in an error, a log line or a diff.
package secretbackend

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// errorDetailMax bounds the outside text (Vault's errors, a command's
// stderr) an error quotes.
const errorDetailMax = 200

// checkKeys rejects a setting the backend does not know (a typo is not
// silently ignored).
func checkKeys(kind string, settings map[string]string, known ...string) error {
	ok := map[string]bool{}
	for _, k := range known {
		ok[k] = true
	}
	var bad []string
	for k := range settings {
		if !ok[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	sort.Strings(known)
	return fmt.Errorf("%s: unknown setting(s) %s (known: %s)", kind, strings.Join(bad, ", "), strings.Join(known, ", "))
}

// timeoutSetting reads the "timeout" setting (a Go duration, default def).
func timeoutSetting(kind string, settings map[string]string, def time.Duration) (time.Duration, error) {
	s := settings["timeout"]
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: timeout %q: want a positive duration such as 10s", kind, s)
	}
	return d, nil
}

// clip shortens outside text quoted in an error to one bounded line.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > errorDetailMax {
		s = s[:errorDetailMax] + "…"
	}
	return s
}
