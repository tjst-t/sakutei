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

// import_test.go — Backlog #22 (v1b): `sakutei import` (reverse-render, §24.4)
// and managed-candidate suggestion.
//
// Tags:
//   [#22-1] import then plan reports no changes (dnszone, nftables, fake incl.
//           per-resource scope and --scope filtering)
//   [#22-2] fields that change between observations are marked as managed
//           candidates (library level, reality mutated between samples)
//   [#22-3] safety: ExplicitAbsentOnly on every resource; incomplete List →
//           error, nothing written; unknown resource skipped; secret plaintext
//           never exported; no overwrite without --force
//   [#22-4] import never changes reality

package acceptance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/importer"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// runSakutei runs the binary with args and returns combined output and exit code.
func runSakutei(t *testing.T, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(buildBinary(t), args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("run sakutei %v: %v", args, err)
	}
	return string(out), 0
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertPlanConverged(t *testing.T, tag string, args ...string) {
	t.Helper()
	out, code := runSakutei(t, append([]string{"plan"}, args...)...)
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("%s plan right after import must report no changes (exit %d):\n%s", tag, code, out)
	}
}

func TestImport_DNSZone_PlanIsConverged(t *testing.T) {
	// [#22-1] [#22-3] [#22-4]
	tmp := t.TempDir()
	zone := filepath.Join(tmp, "zone.db")
	reality := "www.example.com. 300 IN A 192.0.2.10\napi.example.com. 300 IN A 192.0.2.20\n"
	writeStateJSON(t, zone, reality)
	intent := filepath.Join(tmp, "intent")

	out, code := runSakutei(t, "import", "--provider", "dnszone", "--zone", zone, "--out", intent, "--samples", "2", "--sample-interval", "10ms")
	if code != 0 {
		t.Fatalf("[#22-1] import failed (exit %d):\n%s", code, out)
	}
	yaml := readFile(t, filepath.Join(intent, "imported-dnszone.yaml"))
	if strings.Count(yaml, `deletion_policy: "ExplicitAbsentOnly"`) != 2 {
		t.Errorf("[#22-3] every imported resource must be ExplicitAbsentOnly (no prune before review):\n%s", yaml)
	}
	if readFile(t, zone) != reality {
		t.Errorf("[#22-4] import must not change reality")
	}
	assertPlanConverged(t, "[#22-1]", "--intent", intent, "--zone", zone)
}

func TestImport_NFTables_PlanIsConverged(t *testing.T) {
	// [#22-1]
	tmp := t.TempDir()
	rules := filepath.Join(tmp, "fw.rules")
	writeStateJSON(t, rules, "# managed\ntable ip filter {\n\tchain input {\n\t\tip saddr 10.0.0.1 accept\n\t\ttcp dport 22 accept\n\t}\n}\n")
	intent := filepath.Join(tmp, "intent")

	out, code := runSakutei(t, "import", "--provider", "nftables", "--ruleset", rules, "--out", intent, "--samples", "1")
	if code != 0 {
		t.Fatalf("[#22-1] import failed (exit %d):\n%s", code, out)
	}
	assertPlanConverged(t, "[#22-1]", "--intent", intent, "--state", rules)
}

func TestImport_Fake_ScopeAndFilter(t *testing.T) {
	// [#22-1]
	tmp := t.TempDir()
	state := filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{
  "resources": {
    "vm-1": {"fields": {"name": "vm-1", "size": 1}},
    "vm-2": {"fields": {"name": "vm-2", "size": 2}},
    "vm-3": {"fields": {"name": "vm-3", "size": 3}}
  },
  "scopes": {"vm-1": {"site": "tokyo"}, "vm-2": {"site": "osaka"}, "vm-3": {"site": "tokyo"}},
  "provider_version": "v-test"
}`)

	// Whole reality.
	all := filepath.Join(tmp, "intent-all")
	if out, code := runSakutei(t, "import", "--provider", "fake", "--state", state, "--out", all, "--samples", "1"); code != 0 {
		t.Fatalf("[#22-1] import failed:\n%s", out)
	}
	yaml := readFile(t, filepath.Join(all, "imported-fake.yaml"))
	if !strings.Contains(yaml, "scope:\n      site: \"tokyo\"") {
		t.Errorf("[#22-1] per-resource scope must be exported:\n%s", yaml)
	}
	assertPlanConverged(t, "[#22-1]", "--intent", all, "--state", state)

	// Only site=tokyo.
	tokyo := filepath.Join(tmp, "intent-tokyo")
	if out, code := runSakutei(t, "import", "--provider", "fake", "--state", state, "--out", tokyo, "--samples", "1", "--scope", "site=tokyo"); code != 0 {
		t.Fatalf("[#22-1] import --scope failed:\n%s", out)
	}
	yaml = readFile(t, filepath.Join(tokyo, "imported-fake.yaml"))
	if !strings.Contains(yaml, `id: "vm-1"`) || !strings.Contains(yaml, `id: "vm-3"`) || strings.Contains(yaml, `id: "vm-2"`) {
		t.Errorf("[#22-1] --scope site=tokyo must export vm-1 and vm-3 only:\n%s", yaml)
	}
}

func TestImport_ManagedCandidates(t *testing.T) {
	// [#22-2]
	state := filepath.Join(t.TempDir(), "state.json")
	write := func(size int) {
		writeStateJSON(t, state, `{"resources": {"vm-1": {"fields": {"name": "vm-1", "size": `+strconv.Itoa(size)+`}}, "vm-2": {"fields": {"name": "vm-2", "size": 7}}}, "provider_version": "v-test"}`)
	}
	write(1)
	sample := 1
	res, err := importer.Run(context.Background(), importer.Options{
		Providers: map[string]provider.Provider{"fake": fakeprovider.New(state)},
		Samples:   3,
		Sleep: func(time.Duration) { // reality changes between observations
			sample++
			write(sample)
		},
	})
	if err != nil {
		t.Fatalf("[#22-2] import: %v", err)
	}
	byID := map[string]importer.Resource{}
	for _, r := range res.Resources {
		byID[r.ID.Local] = r
	}
	vm1 := byID["vm-1"]
	if got := vm1.ManagedCandidates["size"]; strings.Join(got, ",") != "1,2,3" {
		t.Errorf("[#22-2] vm-1 size changed 1→2→3 and must be a managed candidate; got %v", got)
	}
	if _, ok := vm1.ManagedCandidates["name"]; ok {
		t.Errorf("[#22-2] vm-1 name did not change and must not be a candidate")
	}
	if len(byID["vm-2"].ManagedCandidates) != 0 {
		t.Errorf("[#22-2] vm-2 did not change; got candidates %v", byID["vm-2"].ManagedCandidates)
	}
	if vm1.Fields["size"] != "3" {
		t.Errorf("[#22-2] exported value must be the last observation (3); got %q", vm1.Fields["size"])
	}
	yaml := string(importer.EmitYAML(res.Resources, 3))
	if !strings.Contains(yaml, "# managed 候補: 観測のたびに値が変化（\"1\" → \"2\" → \"3\"）\n      size: \"3\"") {
		t.Errorf("[#22-2] the YAML must mark the candidate right above the field:\n%s", yaml)
	}
}

func TestImport_IncompleteListRefused(t *testing.T) {
	// [#22-3]
	tmp := t.TempDir()
	state := filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{"resources": {"vm-1": {"fields": {"name": "vm-1"}}}, "list_partial": true, "provider_version": "v-test"}`)
	intent := filepath.Join(tmp, "intent")
	out, code := runSakutei(t, "import", "--provider", "fake", "--state", state, "--out", intent, "--samples", "1")
	if code == 0 || !strings.Contains(out, "not complete") {
		t.Fatalf("[#22-3] an incomplete List must abort the import (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(intent, "imported-fake.yaml")); err == nil {
		t.Errorf("[#22-3] nothing may be written when the import is refused")
	}
}

func TestImport_UnknownResourceSkipped(t *testing.T) {
	// [#22-3]
	tmp := t.TempDir()
	state := filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{"resources": {"vm-1": {"fields": {"name": "vm-1"}}, "vm-2": {"fields": {"name": "vm-2"}}}, "observe_unknown": {"vm-2": "unreachable"}, "provider_version": "v-test"}`)
	intent := filepath.Join(tmp, "intent")
	out, code := runSakutei(t, "import", "--provider", "fake", "--state", state, "--out", intent, "--samples", "1")
	if code != 0 {
		t.Fatalf("[#22-3] import failed:\n%s", out)
	}
	yaml := readFile(t, filepath.Join(intent, "imported-fake.yaml"))
	if strings.Contains(yaml, `id: "vm-2"`) {
		t.Errorf("[#22-3] an unobservable resource must not be exported (unknown ≠ absent, and its state is unknown):\n%s", yaml)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "vm-2") {
		t.Errorf("[#22-3] the skipped resource must be reported:\n%s", out)
	}
}

func TestImport_RefusesOverwrite(t *testing.T) {
	// [#22-3]
	tmp := t.TempDir()
	state := filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{"resources": {"vm-1": {"fields": {"name": "vm-1"}}}, "provider_version": "v-test"}`)
	intent := filepath.Join(tmp, "intent")
	args := []string{"import", "--provider", "fake", "--state", state, "--out", intent, "--samples", "1"}
	if out, code := runSakutei(t, args...); code != 0 {
		t.Fatalf("first import failed:\n%s", out)
	}
	edited := "# hand-edited\n" + readFile(t, filepath.Join(intent, "imported-fake.yaml"))
	writeStateJSON(t, filepath.Join(intent, "imported-fake.yaml"), edited)

	if out, code := runSakutei(t, args...); code == 0 || !strings.Contains(out, "--force") {
		t.Fatalf("[#22-3] a second import must refuse to overwrite (exit %d):\n%s", code, out)
	}
	if readFile(t, filepath.Join(intent, "imported-fake.yaml")) != edited {
		t.Errorf("[#22-3] the hand-edited file must be untouched")
	}
	if out, code := runSakutei(t, append(args, "--force")...); code != 0 {
		t.Fatalf("[#22-3] --force must overwrite:\n%s", out)
	}
}

// secretProvider is a minimal provider with a secret field, to check that
// import never exports a plaintext secret.
type secretProvider struct{ value string }

func (s *secretProvider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		"name":     {Kind: core.FieldKindString, Identity: true, Required: true},
		"password": {Kind: core.FieldKindString, Secret: true},
		"serial":   {Kind: core.FieldKindString, Computed: true},
	}}
}
func (s *secretProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, List: true, ProviderVersion: "v-test"}
}
func (s *secretProvider) Read(_ context.Context, id string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now(), State: core.State{
		"name":     {Kind: core.FieldKindString, Value: id},
		"password": {Kind: core.FieldKindString, Value: s.value},
		"serial":   {Kind: core.FieldKindString, Value: "SN-123"},
	}}, nil
}
func (s *secretProvider) Apply(_ context.Context, _ core.Plan) error { return nil }
func (s *secretProvider) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	return core.ListResult{IDs: []core.ID{core.NewID("sec", core.NewScope(nil), "db-1")}, Status: core.ListComplete}, nil
}

func TestImport_SecretNeverExportedInPlaintext(t *testing.T) {
	// [#22-3]
	for _, tc := range []struct {
		name, value, wantField string
	}{
		{"plaintext observed", "hunter2-plaintext", ""},
		{"SecretRef observed", "secretref:file/db/root#gen3", "secretref:file/db/root#gen3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := importer.Run(context.Background(), importer.Options{
				Providers: map[string]provider.Provider{"sec": &secretProvider{value: tc.value}},
				Samples:   1,
			})
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			yaml := string(importer.EmitYAML(res.Resources, 1))
			if tc.wantField == "" {
				if strings.Contains(yaml, tc.value) {
					t.Fatalf("[#22-3] plaintext secret leaked into import output:\n%s", yaml)
				}
				if !strings.Contains(yaml, "書き出していないフィールド password") {
					t.Errorf("[#22-3] the omitted secret field must be noted:\n%s", yaml)
				}
			} else if !strings.Contains(yaml, `password: "`+tc.wantField+`"`) {
				t.Errorf("[#22-3] a SecretRef must be exported in its reference form:\n%s", yaml)
			}
			if strings.Contains(yaml, "SN-123") {
				t.Errorf("[#22-3] computed fields must not be exported:\n%s", yaml)
			}
		})
	}
}
