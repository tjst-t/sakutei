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

// credential_split_test.go — Backlog #185 (§20.7): the observe and the write
// credential of a remote host.  The sakutei binary runs dnszone on a host
// reached through a fake ssh, which records the user and the key of each
// call and runs the command here (the zone file lives in a temp dir).  Real
// sshd hosts with a read-only user are exercised by tests/integration/remote
// (TestObserveWithReadOnlyUser).
//
// AC tags:
//
//	AC-185-3-1  with only the observe key in the store, status, plan and
//	            reconcile --mode alert-only / --freeze / a --freeze-state freeze
//	            start and observe, as
//	            the observe user with the observe key only; nothing is written
//	AC-185-3-2  a reconcile that may write refuses to start without the write
//	            key, and nothing is done
//	AC-185-3-3  with both keys, the Apply uses the write user and key, the
//	            observation the observe ones; the write key file is not left
//	AC-185-3-4  access cannot be set in --provider-config
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// splitFakeSSH records "<user> <key>" (key: ro / rw / none) of each call and
// runs the remote command with sh.
const splitFakeSSH = `#!/bin/sh
user=; key=; prev=
for a; do
  [ "$prev" = "-l" ] && user=$a
  [ "$prev" = "-i" ] && key=$a
  prev=$a
  last=$a
done
k=none
if [ -n "$key" ]; then
  if grep -q cmVhZC13cml0ZQ "$key"; then k=rw; elif grep -q cmVhZC1vbmx5 "$key"; then k=ro; else k=other; fi
fi
echo "$user $k" >> "$SPLIT_SSH_LOG"
exec sh -c "$last"
`

const (
	splitROKey = "-----BEGIN OPENSSH PRIVATE KEY-----\\ncmVhZC1vbmx5\\n-----END OPENSSH PRIVATE KEY-----"
	splitRWKey = "-----BEGIN OPENSSH PRIVATE KEY-----\\ncmVhZC13cml0ZQ==\\n-----END OPENSSH PRIVATE KEY-----"
)

type splitRig struct {
	t                *testing.T
	bin, dir, zone   string
	ctrl             string
	log              string
	roOnly, bothKeys string // --provider-config
}

func newSplitRig(t *testing.T) *splitRig {
	t.Helper()
	r := &splitRig{t: t, bin: buildBinary(t), dir: t.TempDir()}
	r.zone = filepath.Join(r.dir, "zones", "example.com.zone")
	r.log = filepath.Join(r.dir, "ssh.log")
	t.Setenv("SPLIT_SSH_LOG", r.log)
	if err := os.MkdirAll(filepath.Dir(r.zone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.zone, []byte("$ORIGIN example.com.\n@ 300 IN SOA ns1 admin 2026102700 3600 900 604800 300\n@ 300 IN NS ns1.example.com.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ssh := filepath.Join(r.dir, "ssh")
	if err := os.WriteFile(ssh, []byte(splitFakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	ctrl, err := os.MkdirTemp("/tmp", "scs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ctrl) })
	r.ctrl = ctrl
	if err := os.WriteFile(filepath.Join(r.dir, "known_hosts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, store := range []string{"ro", "both"} {
		secrets := `"ssh/ro": {"value": "` + splitROKey + `", "generation": 1}`
		if store == "both" {
			secrets += `, "ssh/rw": {"value": "` + splitRWKey + `", "generation": 1}`
		}
		if err := os.WriteFile(filepath.Join(r.dir, store+"-secrets.json"), []byte(`{"secrets": {`+secrets+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		hosts := filepath.Join(r.dir, store+"-hosts.json")
		writeJSONFile(t, hosts, map[string]any{
			"ssh": ssh, "secret_store": store + "-secrets.json", "known_hosts": "known_hosts", "control_dir": ctrl,
			"hosts": map[string]any{"ns1": map[string]any{
				"address": "192.0.2.53", "control_persist": "0s",
				"observe": map[string]any{"user": "reader", "private_key": "secretref:file/ssh/ro"},
				"write":   map[string]any{"user": "writer", "private_key": "secretref:file/ssh/rw"},
			}},
		})
		pc := filepath.Join(r.dir, store+"-providers.json")
		writeJSONFile(t, pc, map[string]any{"providers": map[string]any{
			"dnszone": map[string]any{"zone": r.zone, "zone_hosts": "ns1", "remote_config": hosts},
		}})
		if store == "ro" {
			r.roOnly = pc
		} else {
			r.bothKeys = pc
		}
	}
	intentDir := filepath.Join(r.dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{{
		"provider": "dnszone", "id": "ns1/www.example.com./A",
		"fields": map[string]string{"name": "www.example.com.", "type": "A", "value": "192.0.2.10", "ttl": "300"},
	}}})
	return r
}

func (r *splitRig) run(config string, args ...string) (string, int) {
	r.t.Helper()
	full := append(args, "--intent", filepath.Join(r.dir, "intent"), "--state", filepath.Join(r.dir, "fake.json"), "--provider-config", config)
	return runBin(r.t, r.bin, full...)
}

// calls returns the "<user> <key>" lines logged since the last call, and
// clears the log.
func (r *splitRig) calls() []string {
	r.t.Helper()
	b, err := os.ReadFile(r.log)
	if err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	os.Remove(r.log)
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "|"))
}

// freezeState starts a freeze in a --freeze-state file (sakutei freeze start).
func (r *splitRig) freezeState() string {
	r.t.Helper()
	p := filepath.Join(r.dir, "freeze.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if out, code := runBin(r.t, r.bin, "freeze", "start", "--state", p, "--audit-log", filepath.Join(r.dir, "audit.log"), "--reason", "maintenance"); code != 0 {
		r.t.Fatalf("freeze start: exit %d\n%s", code, out)
	}
	return p
}

func (r *splitRig) zoneText() string {
	b, err := os.ReadFile(r.zone)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func TestCredentialSplit_ObserveOnlyRunsWithoutTheWriteKey(t *testing.T) {
	// [AC-185-3-1]
	r := newSplitRig(t)
	before := r.zoneText()
	for _, args := range [][]string{
		{"status", "--json"},
		{"plan"},
		{"reconcile", "--mode", "alert-only"},
		{"reconcile", "--freeze", "--freeze-reason", "maintenance", "--audit-log", filepath.Join(r.dir, "audit.log")},
		{"reconcile", "--freeze-state", r.freezeState(), "--audit-log", filepath.Join(r.dir, "audit.log")},
	} {
		out, code := r.run(r.roOnly, args...)
		if code != 0 {
			t.Fatalf("[AC-185-3-1] %v with the observe key only: exit %d\n%s", args, code, out)
		}
		if !strings.Contains(out, "www.example.com.") && args[0] != "reconcile" {
			t.Errorf("[AC-185-3-1] %v did not observe the record:\n%s", args, out)
		}
		if args[0] == "status" && !strings.Contains(out, `"drift": 1`) {
			t.Errorf("[AC-185-3-1] status: want the record observed as drift:\n%s", out)
		}
		if args[0] == "reconcile" && !strings.Contains(out, "1 drifted resource(s) detected") {
			t.Errorf("[AC-185-3-1] %v:\n%s", args, out)
		}
		calls := r.calls()
		if len(calls) == 0 {
			t.Errorf("[AC-185-3-1] %v: no ssh call", args)
		}
		for _, c := range calls {
			if c != "reader|ro" {
				t.Errorf("[AC-185-3-1] %v: an ssh call as %q; want the observe user and key only", args, c)
			}
		}
	}
	if r.zoneText() != before {
		t.Errorf("[AC-185-3-1] the zone changed in runs that do not write:\n%s", r.zoneText())
	}
}

func TestCredentialSplit_WritingRunNeedsTheWriteKey(t *testing.T) {
	// [AC-185-3-2]
	r := newSplitRig(t)
	before := r.zoneText()
	out, code := r.run(r.roOnly, "reconcile")
	if code == 0 || !strings.Contains(out, "hosts.ns1.write.private_key") || !strings.Contains(out, "not in the secret store") {
		t.Fatalf("[AC-185-3-2] reconcile (auto) without the write key: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "cmVhZC") {
		t.Errorf("[AC-185-3-2] the output quotes a key:\n%s", out)
	}
	if r.zoneText() != before {
		t.Error("[AC-185-3-2] the zone changed")
	}
	if calls := r.calls(); len(calls) != 0 {
		t.Errorf("[AC-185-3-2] ssh was called: %v", calls)
	}
}

func TestCredentialSplit_ApplyUsesTheWriteCredential(t *testing.T) {
	// [AC-185-3-3]
	r := newSplitRig(t)
	out, code := r.run(r.bothKeys, "reconcile")
	if code != 0 {
		t.Fatalf("[AC-185-3-3] reconcile: exit %d\n%s", code, out)
	}
	if !strings.Contains(r.zoneText(), "192.0.2.10") {
		t.Fatalf("[AC-185-3-3] the record was not written:\n%s", r.zoneText())
	}
	seen := map[string]int{}
	for _, c := range r.calls() {
		seen[c]++
	}
	if seen["reader|ro"] == 0 || seen["writer|rw"] == 0 || len(seen) != 2 {
		t.Errorf("[AC-185-3-3] ssh calls %v; want the observe user with the observe key and the write user with the write key, nothing mixed", seen)
	}
	if left, _ := filepath.Glob(filepath.Join(r.ctrl, "key-*")); len(left) != 0 {
		t.Errorf("[AC-185-3-3] key files left: %v", left)
	}
	// Converged: the next status observes with the observe key only.
	out, code = r.run(r.roOnly, "status", "--json")
	if code != 0 || !strings.Contains(out, `"conformant": 1`) {
		t.Errorf("[AC-185-3-3] status after: exit %d\n%s", code, out)
	}
	for _, c := range r.calls() {
		if c != "reader|ro" {
			t.Errorf("[AC-185-3-3] status called ssh as %q", c)
		}
	}
}

func TestCredentialSplit_AccessIsNotAProviderSetting(t *testing.T) {
	// [AC-185-3-4]
	r := newSplitRig(t)
	pc := filepath.Join(r.dir, "access-providers.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{
		"dnszone": map[string]any{"zone": r.zone, "zone_hosts": "ns1", "remote_config": filepath.Join(r.dir, "both-hosts.json"), "access": "observe"},
	}})
	out, code := r.run(pc, "plan")
	if code == 0 || !strings.Contains(out, "providers.dnszone.access") {
		t.Errorf("[AC-185-3-4] access in --provider-config: exit %d\n%s", code, out)
	}
}
