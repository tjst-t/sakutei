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
package loader

import (
	"fmt"
	"time"

	"github.com/tjst-t/sakutei/internal/remediation"
	"github.com/tjst-t/sakutei/pkg/core"
)

// parseRemediation reads the intent's top-level `remediation:` (§14.2).  A
// missing key is nil (no schedule: the schedule is not a decision source).
// An omitted default is alert-only, the most conservative side.
func parseRemediation(v any) (*remediation.Schedule, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("remediation: オブジェクトでなければなりません")
	}
	s := &remediation.Schedule{Default: remediation.ModeAlertOnly, Location: time.UTC}
	if d, _ := m["default"].(string); d != "" {
		mode, err := remediation.ParseMode(d)
		if err != nil {
			return nil, fmt.Errorf("remediation.default: %w", err)
		}
		s.Default = mode
	}
	if tz, _ := m["tz"].(string); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("remediation.tz: %w", err)
		}
		s.Location = loc
	}
	raw, _ := m["schedule"].([]any)
	for i, item := range raw {
		e, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("remediation.schedule[%d]: オブジェクトでなければなりません", i)
		}
		spec, _ := e["window"].(string)
		w, err := remediation.ParseWindow(spec)
		if err != nil {
			return nil, fmt.Errorf("remediation.schedule[%d]: %w", i, err)
		}
		modeStr, _ := e["mode"].(string)
		mode, err := remediation.ParseMode(modeStr)
		if err != nil || modeStr == "" {
			return nil, fmt.Errorf("remediation.schedule[%d]: mode must be auto or alert-only (got %q)", i, modeStr)
		}
		s.Entries = append(s.Entries, remediation.Entry{Window: w, Mode: mode})
	}
	return s, nil
}

// AlertOnlyScopes returns the scopes declared `remediation: alert-only`
// (scope-explicit mode, §14.3).  A resource in one of them is alert-only even
// when every run-wide source allows auto.
func (in Intent) AlertOnlyScopes() []core.Scope {
	var out []core.Scope
	for _, sp := range in.Scopes {
		if sp.Remediation == remediation.ModeAlertOnly.String() {
			out = append(out, sp.Scope)
		}
	}
	return out
}
