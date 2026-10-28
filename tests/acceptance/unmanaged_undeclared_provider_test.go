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

// unmanaged_undeclared_provider_test.go — Backlog #164: sakutei status Lists
// every List-capable provider that is set up for the run (its own flag, or a
// section in --provider-config), also when the intent declares none of its
// resources, and shows what is in reality as unmanaged.  The VM case
// (proxmox-vm) is AC-164-1 in proxmox_vm_test.go.
//
// AC tags:
//
//	AC-164-2  with only fake resources declared, the dnszone records (--zone),
//	          the nftables chains (--ruleset), the s3-bucket buckets
//	          (--s3-config) and the ldap entries (--provider-config) show as
//	          unmanaged; a provider that is not set up is not Listed
//	AC-164-3  a provider set up for the run that cannot be built is reported
//	          in unmanaged_undetectable (list_error, exit 3), never read as
//	          "no unmanaged"
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/ldap/ldaptest"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket/s3test"
)

type unmanagedReport struct {
	Complete bool `json:"complete"`
	Summary  struct {
		Unmanaged  int `json:"unmanaged"`
		Conformant int `json:"conformant"`
	} `json:"summary"`
	Resources []struct {
		Provider string `json:"provider"`
		LocalID  string `json:"local_id"`
		State    string `json:"state"`
	} `json:"resources"`
	Undetectable []struct {
		Provider string `json:"provider"`
		Kind     string `json:"kind"`
	} `json:"unmanaged_undetectable"`
}

// fakeOnlyIntent writes an intent with one fake resource (in reality, so
// conformant) and returns the intent dir and the fake state path.
func fakeOnlyIntent(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "vm1", "fields": map[string]string{"name": "vm1", "size": "20"}},
		},
	})
	statePath := filepath.Join(tmp, "state.json")
	seedState(t, statePath, map[string]map[string]any{"vm1": {"name": "vm1", "size": "20"}})
	return intentDir, statePath
}

func statusUnmanaged(t *testing.T, intentDir, statePath string, args ...string) (unmanagedReport, int) {
	t.Helper()
	stdout, stderr, code := runStatusCmd(t, intentDir, statePath, append([]string{"--json"}, args...)...)
	var rep unmanagedReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("status --json exit %d, not JSON: %v\n%s\nstderr: %s", code, err, stdout, stderr)
	}
	return rep, code
}

func TestStatus_UndeclaredProvidersShowUnmanaged(t *testing.T) {
	// [AC-164-2]
	intentDir, statePath := fakeOnlyIntent(t)
	tmp := t.TempDir()

	// 何も設定しなければ、宣言の無い provider は List しない（今までどおり）。
	rep, code := statusUnmanaged(t, intentDir, statePath)
	if code != 0 || rep.Summary.Unmanaged != 0 || rep.Summary.Conformant != 1 {
		t.Fatalf("[AC-164-2] status with nothing set up: exit %d, %+v", code, rep)
	}

	zonePath := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zonePath, "$ORIGIN example.com.\nwww\t300\tIN\tA\t192.0.2.1\n")
	rulesetPath := filepath.Join(tmp, "managed.rules")
	writeRulesetFile(t, rulesetPath, "table ip filter {\n\tchain allow { ip saddr 192.0.2.1 accept\n\t}\n}\n")
	s3 := s3test.New(s3test.Options{})
	defer s3.Close()
	s3.CreateBucket("left-bucket")
	lab := newLDAPLab(t)
	lab.edit(t, func(st *ldaptest.State) {
		st.Put("ou=orphan,dc=example,dc=com", map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"orphan"}})
	})

	rep, code = statusUnmanaged(t, intentDir, statePath,
		"--zone", zonePath, "--ruleset", rulesetPath, "--s3-config", writeS3Config(t, s3),
		"--provider-config", lab.providerConfig)
	if code != 0 || !rep.Complete || len(rep.Undetectable) != 0 {
		t.Errorf("[AC-164-2] status: exit %d, complete %v, undetectable %+v", code, rep.Complete, rep.Undetectable)
	}
	got := map[string][]string{}
	for _, r := range rep.Resources {
		if r.State == "unmanaged" {
			got[r.Provider] = append(got[r.Provider], r.LocalID)
		}
	}
	for prov, want := range map[string]string{
		"dnszone": "www.example.com./A", "nftables": "ip/filter/allow", "s3-bucket": "fake/left-bucket", "ldap": "dir1/ou=orphan,dc=example,dc=com",
	} {
		if !strings.Contains(strings.Join(got[prov], " "), want) {
			t.Errorf("[AC-164-2] %s: unmanaged %v, want %s among them", prov, got[prov], want)
		}
	}
	if len(got["fake"]) != 0 || rep.Summary.Conformant != 1 {
		t.Errorf("[AC-164-2] the declared fake resource is unmanaged or not conformant: %+v", rep)
	}
}

func TestStatus_UndeclaredProviderThatCannotBeBuiltIsUndetectable(t *testing.T) {
	// [AC-164-3]
	intentDir, statePath := fakeOnlyIntent(t)
	pc := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(pc, []byte("providers:\n  proxmox-vm:\n    proxmox_config: /nonexistent/pve.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, code := statusUnmanaged(t, intentDir, statePath, "--provider-config", pc)
	if code != 3 || rep.Complete {
		t.Errorf("[AC-164-3] exit %d, complete %v; want 3 and false", code, rep.Complete)
	}
	if len(rep.Undetectable) != 1 || rep.Undetectable[0].Provider != "proxmox-vm" || rep.Undetectable[0].Kind != "list_error" {
		t.Errorf("[AC-164-3] unmanaged_undetectable = %+v; want proxmox-vm list_error", rep.Undetectable)
	}
}
