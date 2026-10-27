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

// status_json_test.go — #118: status --json（機械で読める出力、§26.4）。
//
//	AC-114-4-1  drift・観測できない・管理外・適合を含む状況で、--json の出力を
//	            encoding/json で読み、件数と各リソースの状態が文の出力と一致する
//	AC-114-4-2  終了コードは文の出力と同じ（観測できないものがあれば 3、無ければ 0）
//	AC-114-4-3  drift のフィールドの一覧・原因タグ・観測エラーの文が入る
package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type statusJSON struct {
	SchemaVersion int    `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	Complete      bool   `json:"complete"`
	Summary       struct {
		Conformant   int `json:"conformant"`
		Drift        int `json:"drift"`
		Missing      int `json:"missing"`
		Unobservable int `json:"unobservable"`
		Unmanaged    int `json:"unmanaged"`
		Workflow     int `json:"workflow"`
		Total        int `json:"total"`
	} `json:"summary"`
	Resources []struct {
		ID       string            `json:"id"`
		Provider string            `json:"provider"`
		Scope    map[string]string `json:"scope"`
		LocalID  string            `json:"local_id"`
		State    string            `json:"state"`
		Missing  bool              `json:"missing"`
		Cause    string            `json:"cause"`
		Fields   []struct {
			Field string `json:"field"`
			From  string `json:"from"`
			To    string `json:"to"`
		} `json:"fields"`
		ObserveError string `json:"observe_error"`
	} `json:"resources"`
	UnmanagedUndetectable []struct {
		Provider string `json:"provider"`
		Kind     string `json:"kind"`
	} `json:"unmanaged_undetectable"`
}

// setupMixedStatus: bmc-1 (Read error → unobservable), bmc-2 (in sync →
// conformant), stray (fake reality only → unmanaged), www (zone drift),
// mail (zone reality only → unmanaged), db (declared, absent → missing drift).
func setupMixedStatus(t *testing.T, observeErr bool) (intentDir, statePath, zonePath string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	zonePath = filepath.Join(tmp, "example.com.zone")
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "bmc-1", "fields": map[string]string{"name": "bmc-1", "size": "10"}},
			{"provider": "fake", "id": "bmc-2", "fields": map[string]string{"name": "bmc-2", "size": "20"}},
			{"provider": "fake", "id": "db", "fields": map[string]string{"name": "db", "size": "30"}},
			dnszoneResource("www.example.com./A", "www.example.com.", "A", "1.2.3.4", "300", ""),
		},
	})
	seedState(t, statePath, map[string]map[string]any{
		"bmc-1": {"name": "bmc-1", "size": "10"},
		"bmc-2": {"name": "bmc-2", "size": "20"},
		"stray": {"name": "stray", "size": "1"},
	})
	if observeErr {
		setObserveMode(t, statePath, "bmc-1", "error")
	}
	writeZoneFile(t, zonePath, "$ORIGIN example.com.\nwww\t300\tIN\tA\t9.9.9.9\nmail\t300\tIN\tA\t5.6.7.8\n")
	return intentDir, statePath, zonePath
}

var textStateLine = regexp.MustCompile(`(?m)^  (DRIFTED|UNOBSERVABLE|UNMANAGED|WORKFLOW) (\S+)$`)
var textSummary = regexp.MustCompile(`要約: drifted=(\d+) converged=(\d+) unobservable=(\d+) unmanaged=(\d+)`)

// TestStatusJSON_MatchesText: the JSON report agrees with the text report.
//
// [AC-114-4-1] [AC-114-4-2] [AC-114-4-3]
func TestStatusJSON_MatchesText(t *testing.T) {
	for _, observeErr := range []bool{true, false} {
		t.Run(fmt.Sprintf("observeErr=%t", observeErr), func(t *testing.T) {
			// [AC-114-4-1]
			intentDir, statePath, zonePath := setupMixedStatus(t, observeErr)
			text, _, textCode := runStatusCmd(t, intentDir, statePath, "--zone", zonePath)
			out, stderr, jsonCode := runStatusCmd(t, intentDir, statePath, "--zone", zonePath, "--json")

			// [AC-114-4-2]
			wantCode := 0
			if observeErr {
				wantCode = 3
			}
			if textCode != wantCode || jsonCode != textCode {
				t.Fatalf("[AC-114-4-2] exit codes: text=%d json=%d, want %d; stderr: %s", textCode, jsonCode, wantCode, stderr)
			}

			var got statusJSON
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("[AC-114-4-1] stdout is not one JSON object: %v\n%s", err, out)
			}
			if got.SchemaVersion != 1 || got.GeneratedAt == "" || got.Complete == observeErr {
				t.Errorf("[AC-114-4-1] header: %+v", got)
			}

			m := textSummary.FindStringSubmatch(text)
			if m == nil {
				t.Fatalf("no summary in text output:\n%s", text)
			}
			s := got.Summary
			gotSummary := fmt.Sprintf("%d %d %d %d", s.Drift, s.Conformant, s.Unobservable, s.Unmanaged)
			if want := strings.Join(m[1:], " "); gotSummary != want {
				t.Errorf("[AC-114-4-1] summary drift/conformant/unobservable/unmanaged: json %s, text %s", gotSummary, want)
			}
			if s.Total != len(got.Resources) || s.Total != s.Drift+s.Conformant+s.Unobservable+s.Unmanaged+s.Workflow {
				t.Errorf("[AC-114-4-1] total %d does not match resources %d / counts %+v", s.Total, len(got.Resources), s)
			}

			// Per-resource state: the text's DRIFTED/UNOBSERVABLE/UNMANAGED lines
			// and the JSON's states must name the same resources.
			textStates := map[string]string{}
			for _, mm := range textStateLine.FindAllStringSubmatch(text, -1) {
				textStates[mm[2]] = map[string]string{
					"DRIFTED": "drift", "UNOBSERVABLE": "unobservable", "UNMANAGED": "unmanaged", "WORKFLOW": "workflow",
				}[mm[1]]
			}
			jsonStates := map[string]string{}
			for _, r := range got.Resources {
				if r.State == "conformant" {
					continue
				}
				jsonStates[r.ID] = r.State
			}
			if fmt.Sprint(textStates) != fmt.Sprint(jsonStates) {
				t.Errorf("[AC-114-4-1] states differ:\ntext %v\njson %v", textStates, jsonStates)
			}

			byLocal := map[string]int{}
			for i, r := range got.Resources {
				byLocal[r.LocalID] = i
			}
			wantStates := map[string]string{
				"bmc-2": "conformant", "db": "drift", "stray": "unmanaged",
				"www.example.com./A": "drift", "mail.example.com./A": "unmanaged",
			}
			if observeErr {
				wantStates["bmc-1"] = "unobservable"
			} else {
				wantStates["bmc-1"] = "conformant"
			}
			for local, want := range wantStates {
				i, ok := byLocal[local]
				if !ok || got.Resources[i].State != want {
					t.Errorf("[AC-114-4-1] %s: want state %s; resources: %+v", local, want, got.Resources)
				}
			}

			// [AC-114-4-3]
			if i, ok := byLocal["www.example.com./A"]; ok {
				r := got.Resources[i]
				if r.Provider != "dnszone" || r.Cause == "" || r.Missing || len(r.Fields) == 0 {
					t.Errorf("[AC-114-4-3] www drift should carry provider, cause and fields: %+v", r)
				}
				if !strings.Contains(text, "原因タグ: "+r.Cause) {
					t.Errorf("[AC-114-4-3] cause %q not the text's; text:\n%s", r.Cause, text)
				}
				for _, f := range r.Fields {
					if f.Field == "value" && (f.From != "9.9.9.9" || f.To != "1.2.3.4") {
						t.Errorf("[AC-114-4-3] value field: %+v", f)
					}
				}
			}
			if i, ok := byLocal["db"]; ok && (!got.Resources[i].Missing || s.Missing != 1) {
				t.Errorf("[AC-114-4-3] db should be a missing drift: %+v (summary %+v)", got.Resources[i], s)
			}
			if observeErr {
				r := got.Resources[byLocal["bmc-1"]]
				if !strings.Contains(r.ObserveError, "injected observe error") || !strings.Contains(text, r.ObserveError) {
					t.Errorf("[AC-114-4-3] bmc-1 observe error should match the text's: %+v", r)
				}
			}
			assertNoApplyVerbs(t, "[AC-114-4-1]", out)
		})
	}
}
