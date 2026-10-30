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

// s3bucket_object_lock_test.go — Backlog #205: s3-bucket's Object Lock (the
// enable at create and the default retention), through the CLI against the
// fake S3 server shaped like an implementation without the create-time
// enable, whose versioning Get is 501, that accepts only COMPLIANCE and
// answers a Years retention with Days.
//
// AC tags:
//
//	AC-205-1  PutObjectLockConfiguration alone makes a new bucket what the
//	          intent declares (no create-time enable, versioning never
//	          needed), and the years in the intent match the Days the
//	          endpoint reports: the next reconcile is converged and status
//	          reports the bucket conformant, with versioning (501) not an error
//	AC-205-2  a mode the endpoint does not accept (GOVERNANCE) stops with
//	          the reason (a PermanentError naming InvalidArgument) and nothing
//	          is kept
//	AC-205-3  changing the retention of an existing bucket — shortening it
//	          and removing it — is held for approval without an open
//	          planned_change lane (ChangeAwaitingApproval, the endpoint
//	          unchanged), and applied with an approved lane of kind update
package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket/s3test"
)

type lockRig struct {
	t      *testing.T
	srv    *s3test.Server
	bin    string
	dir    string
	common []string
	audit  string
	now    time.Time
}

func newLockRig(t *testing.T) *lockRig {
	t.Helper()
	srv := s3test.New(s3test.Options{
		NoObjectLockOnCreate:  true,
		NotImplemented:        []string{s3test.APIVersioning},
		ObjectLockModes:       []string{s3bucket.ObjectLockCompliance},
		ObjectLockYearsAsDays: true,
	})
	t.Cleanup(srv.Close)
	cfg := writeS3ConfigWith(t, srv, "    fields:\n      - versioning\n      - tags\n      - object_lock\n")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("scope: site=lab\n"+string(body)), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &lockRig{t: t, srv: srv, bin: buildBinary(t), dir: dir, now: time.Now().UTC(), audit: filepath.Join(tmp, "audit.jsonl"),
		common: []string{"--intent", dir, "--state", filepath.Join(tmp, "fake.json"), "--s3-config", cfg}}
}

func lockBucket(name, objectLock string) map[string]any {
	return map[string]any{"provider": s3bucket.Name, "scope": map[string]string{"site": "lab"}, "id": "fake/" + name,
		"fields": map[string]string{s3bucket.FieldObjectLock: objectLock}}
}

// write writes the intent; lanes are approved planned_change lanes over
// site=lab of the given kinds.
func (r *lockRig) write(resources []map[string]any, kinds ...string) {
	r.t.Helper()
	intent := map[string]any{"resources": resources}
	var lanes []map[string]any
	for _, k := range kinds {
		lanes = append(lanes, map[string]any{"scope": map[string]string{"site": "lab"}, "kind": k, "max_count": 2,
			"window":   pcWindow(r.now.Add(-time.Hour), r.now.Add(time.Hour)),
			"approval": map[string]any{"pr": 205, "approvers": []string{"alice"}}})
	}
	if len(lanes) > 0 {
		intent["planned_change"] = lanes
	}
	writeIntentFile(r.t, r.dir, intent)
}

func (r *lockRig) run(args ...string) (string, int) {
	r.t.Helper()
	if args[0] == "reconcile" {
		args = append(args, "--audit-log", r.audit)
	}
	return runBin(r.t, r.bin, append(args, r.common...)...)
}

func (r *lockRig) lock(bucket string) string {
	mode, days, years, ok := r.srv.ObjectLock(bucket)
	if !ok {
		return "off"
	}
	if mode == "" {
		return "on"
	}
	if years > 0 {
		return mode + " " + strconv.Itoa(years) + "y"
	}
	return mode + " " + strconv.Itoa(days) + "d"
}

func TestS3Bucket_ObjectLock(t *testing.T) {
	r := newLockRig(t)
	seven := `{"mode": "COMPLIANCE", "years": 7}`

	// [AC-205-1]
	r.write([]map[string]any{lockBucket("vault", seven)})
	if o, code := r.run("reconcile"); code != 0 {
		t.Fatalf("[AC-205-1] reconcile exit %d:\n%s", code, o)
	}
	if got := r.lock("vault"); got != "COMPLIANCE 2555d" {
		t.Fatalf("[AC-205-1] the endpoint keeps %q; want COMPLIANCE 2555d", got)
	}
	if puts := r.srv.LockPuts(); len(puts) != 1 || !strings.Contains(puts[0], "<Years>7</Years>") {
		t.Errorf("[AC-205-1] PutObjectLockConfiguration bodies = %v; want one with Years 7", puts)
	}
	if o, code := r.run("reconcile"); code != 0 || !strings.Contains(o, "Converged (no changes)") {
		t.Fatalf("[AC-205-1] second reconcile exit %d, want converged (years 7 = 2555 days):\n%s", code, o)
	}
	c, u, raw := s3StatusArgs(t, r.bin, r.common)
	if c != 1 || u != 0 {
		t.Fatalf("[AC-205-1] status: conformant=%d unobservable=%d; want 1 and 0\n%s", c, u, raw)
	}

	// [AC-205-2]
	r.write([]map[string]any{lockBucket("vault", seven), lockBucket("governed", `{"mode": "GOVERNANCE", "days": 1}`)})
	o, _ := r.run("reconcile")
	if !strings.Contains(o, "refused the Object Lock configuration") || !strings.Contains(o, "InvalidArgument") || !strings.Contains(o, "permanent") {
		t.Errorf("[AC-205-2] reconcile did not stop with the reason:\n%s", o)
	}
	if got := r.lock("governed"); got != "off" {
		t.Errorf("[AC-205-2] governed = %q; want off", got)
	}

	// [AC-205-3] 短くする：レーンが無ければ流れない。
	shorter := []map[string]any{lockBucket("vault", `{"mode": "COMPLIANCE", "days": 30}`)}
	r.write(shorter)
	if o, code := r.run("plan"); code != 0 || !strings.Contains(o, "blocked: approval") || !strings.Contains(o, "shortens the default retention 2555 → 30 days") {
		t.Errorf("[AC-205-3] plan without a lane exit %d:\n%s", code, o)
	}
	o, _ = r.run("reconcile")
	if !strings.Contains(o, "ChangeAwaitingApproval") || r.lock("vault") != "COMPLIANCE 2555d" {
		t.Fatalf("[AC-205-3] shortening without approval: %q\n%s", r.lock("vault"), o)
	}
	r.write(shorter, "update")
	if o, code := r.run("reconcile"); code != 0 || r.lock("vault") != "COMPLIANCE 30d" {
		t.Fatalf("[AC-205-3] shortening with an approved lane exit %d: %q\n%s", code, r.lock("vault"), o)
	}

	// 外す：同じ。
	removed := []map[string]any{lockBucket("vault", `{}`)}
	r.write(removed)
	o, _ = r.run("reconcile")
	if !strings.Contains(o, "ChangeAwaitingApproval") || !strings.Contains(o, "removes the default retention") || r.lock("vault") != "COMPLIANCE 30d" {
		t.Fatalf("[AC-205-3] removing without approval: %q\n%s", r.lock("vault"), o)
	}
	r.write(removed, "update")
	if o, code := r.run("reconcile"); code != 0 || r.lock("vault") != "on" {
		t.Fatalf("[AC-205-3] removing with an approved lane exit %d: %q\n%s", code, r.lock("vault"), o)
	}
	if o, code := r.run("reconcile"); code != 0 || !strings.Contains(o, "Converged (no changes)") {
		t.Errorf("[AC-205-3] after the change exit %d, want converged:\n%s", code, o)
	}
}

// s3StatusArgs runs sakutei status --json with the given common flags.
func s3StatusArgs(t *testing.T, bin string, common []string) (conformant, unobservable int, raw string) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"status", "--json"}, common...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, _ := cmd.Output()
	var doc struct {
		Summary struct {
			Conformant   int `json:"conformant"`
			Unobservable int `json:"unobservable"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("status --json: %v\n%s\n%s", err, out, stderr.String())
	}
	return doc.Summary.Conformant, doc.Summary.Unobservable, string(out) + stderr.String()
}
