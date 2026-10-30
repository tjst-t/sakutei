//go:build integration_s3

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

package s3

// Object Lock（#205）を本物のサーバで確かめる。seaweedfs（必須）と、使えれば
// minio に、object_lock を fields に書いた endpoint（bucket_prefix sk-lock-）を
// 足し、作る・観測する・保持の設定を変える（承認のレーンが要る）を回す。
// minio では、観測用の利用者（GetBucketObjectLockConfiguration だけ）で Get が
// 通り Put が断られることも確かめる。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
)

type lockLab struct {
	dir, cfg string
	servers  []string
}

func (l lockLab) intent(t *testing.T, kinds []string, rs ...resource) {
	t.Helper()
	var res []map[string]any
	for _, r := range rs {
		res = append(res, map[string]any{"provider": r.Provider, "id": r.ID, "scope": map[string]string{"site": "it"}, "fields": r.Fields})
	}
	in := map[string]any{"resources": res}
	now := time.Now().UTC()
	var lanes []map[string]any
	for _, k := range kinds {
		lanes = append(lanes, map[string]any{"scope": map[string]string{"site": "it"}, "kind": k, "max_count": 10,
			"window":   now.Add(-time.Hour).Format(time.RFC3339) + " .. " + now.Add(time.Hour).Format(time.RFC3339),
			"approval": map[string]any{"pr": 205, "approvers": []string{"alice"}}})
	}
	if len(lanes) > 0 {
		in["planned_change"] = lanes
	}
	b, _ := json.MarshalIndent(in, "", "  ")
	if err := os.WriteFile(filepath.Join(l.dir, "intent", "buckets.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (l lockLab) run(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{verb,
		"--intent", filepath.Join(l.dir, "intent"),
		"--state", filepath.Join(l.dir, "fake-state.json"),
		"--s3-config", l.cfg,
	}, extra...)
	if verb == "reconcile" {
		args = append(args, "--audit-log", filepath.Join(l.dir, "audit.jsonl"))
	}
	out, err := exec.Command(lab.bin, args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out), 0
}

func (l lockLab) status(t *testing.T) statusDoc {
	t.Helper()
	args := []string{"status", "--json", "--intent", filepath.Join(l.dir, "intent"),
		"--state", filepath.Join(l.dir, "fake-state.json"), "--s3-config", l.cfg}
	out, _ := exec.Command(lab.bin, args...).Output()
	var doc statusDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	return doc
}

func lockOf(t *testing.T, server, bucket string) s3bucket.ObjectLock {
	t.Helper()
	ol, err := session(t, server).GetObjectLock(context.Background(), bucket)
	if err != nil {
		t.Fatalf("%s: GetObjectLockConfiguration %s: %v", server, bucket, err)
	}
	return ol
}

func TestS3ObjectLock(t *testing.T) {
	ctx := context.Background()
	l := lockLab{dir: filepath.Join(lab.dir, "lock")}
	if err := os.MkdirAll(filepath.Join(l.dir, "intent"), 0o755); err != nil {
		t.Fatal(err)
	}
	var cfg strings.Builder
	cfg.WriteString("secret_store: ../secrets.json\nscope: site=it\nendpoints:\n")
	for _, n := range []string{"seaweedfs", "minio"} {
		if s, ok := lab.servers[n]; ok {
			l.servers = append(l.servers, n)
			fmt.Fprintf(&cfg, "  %s:\n    endpoint: %s\n    access_key: secretref:file/s3/access_key\n    secret_key: secretref:file/s3/secret_key\n"+
				"    bucket_prefix: sk-lock-\n    timeout: 5s\n    fields:\n      - versioning\n      - policy\n      - lifecycle\n      - tags\n      - object_lock\n", n, s.url)
		}
	}
	l.cfg = filepath.Join(l.dir, "s3.yaml")
	if err := os.WriteFile(l.cfg, []byte(cfg.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	buckets := func(fields map[string]map[string]string) []resource {
		var rs []resource
		for _, ep := range l.servers {
			for b, f := range fields {
				rs = append(rs, resource{Provider: s3bucket.Name, ID: ep + "/" + b, Fields: f})
			}
		}
		return rs
	}
	n := len(l.servers)
	gov1 := map[string]string{"object_lock": `{"enable_on_create": true, "mode": "GOVERNANCE", "days": 1}`}
	years := map[string]string{"object_lock": `{"enable_on_create": true, "mode": "COMPLIANCE", "years": 1}`}

	t.Run("create with Object Lock and observe it", func(t *testing.T) {
		l.intent(t, nil, buckets(map[string]map[string]string{"sk-lock-vault": gov1, "sk-lock-years": years})...)
		if out, code := l.run(t, "reconcile"); code != 0 {
			t.Fatalf("reconcile exit %d:\n%s", code, out)
		}
		for _, ep := range l.servers {
			if ol := lockOf(t, ep, "sk-lock-vault"); !ol.Enabled || ol.Mode != "GOVERNANCE" || ol.Days != 1 {
				t.Errorf("%s: sk-lock-vault = %+v", ep, ol)
			}
			ol := lockOf(t, ep, "sk-lock-years")
			t.Logf("%s keeps a Years 1 retention as %+v", ep, ol)
			if !ol.Enabled || ol.Mode != "COMPLIANCE" || (ol.Years != 1 && ol.Days != 365) {
				t.Errorf("%s: sk-lock-years = %+v", ep, ol)
			}
			if v, err := session(t, ep).GetVersioning(ctx, "sk-lock-vault"); err != nil || v != "Enabled" {
				t.Logf("%s: versioning of a bucket created with Object Lock = %q, %v", ep, v, err)
			}
		}
		if s := l.status(t); s.Summary.Conformant != 2*n || s.Summary.Drift != 0 || !s.Complete {
			t.Fatalf("status: %+v", s)
		}
		for i := 0; i < 3; i++ {
			if out, code := l.run(t, "reconcile"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
				t.Fatalf("cycle %d exit %d, want converged:\n%s", i, code, out)
			}
		}
	})

	t.Run("changing the retention needs approval", func(t *testing.T) {
		gov2 := map[string]string{"object_lock": `{"enable_on_create": true, "mode": "GOVERNANCE", "days": 2}`}
		l.intent(t, nil, buckets(map[string]map[string]string{"sk-lock-vault": gov2, "sk-lock-years": years})...)
		out, _ := l.run(t, "reconcile")
		if !strings.Contains(out, "ChangeAwaitingApproval") || !strings.Contains(out, "lengthens the default retention 1 → 2 days") {
			t.Errorf("reconcile without a lane:\n%s", out)
		}
		for _, ep := range l.servers {
			if ol := lockOf(t, ep, "sk-lock-vault"); ol.Days != 1 {
				t.Errorf("%s: changed without approval: %+v", ep, ol)
			}
		}
		l.intent(t, []string{"update"}, buckets(map[string]map[string]string{"sk-lock-vault": gov2, "sk-lock-years": years})...)
		if out, code := l.run(t, "reconcile"); code != 0 {
			t.Fatalf("reconcile with an approved lane exit %d:\n%s", code, out)
		}
		for _, ep := range l.servers {
			if ol := lockOf(t, ep, "sk-lock-vault"); ol.Mode != "GOVERNANCE" || ol.Days != 2 {
				t.Errorf("%s: after the approved change: %+v", ep, ol)
			}
		}
		// 既定の保持を外す（承認のレーンつき）。
		none := map[string]string{"object_lock": `{"enable_on_create": true}`}
		l.intent(t, []string{"update"}, buckets(map[string]map[string]string{"sk-lock-vault": none, "sk-lock-years": years})...)
		if out, code := l.run(t, "reconcile"); code != 0 {
			t.Fatalf("removing the retention exit %d:\n%s", code, out)
		}
		for _, ep := range l.servers {
			if ol := lockOf(t, ep, "sk-lock-vault"); !ol.Enabled || ol.Mode != "" {
				t.Errorf("%s: after removing the retention: %+v", ep, ol)
			}
		}
		if s := l.status(t); s.Summary.Conformant != 2*n || s.Summary.Drift != 0 {
			t.Fatalf("status: %+v", s)
		}
	})

	t.Run("an existing bucket without Object Lock", func(t *testing.T) {
		for _, ep := range l.servers {
			if err := session(t, ep).CreateBucket(ctx, "sk-lock-plain"); err != nil {
				t.Fatal(err)
			}
		}
		base := map[string]map[string]string{"sk-lock-vault": {"object_lock": "{}"}, "sk-lock-years": years}
		// enable_on_create は作るときだけ：食い違いとして止まり、作り直さない。
		base["sk-lock-plain"] = map[string]string{"object_lock": `{"enable_on_create": true}`}
		l.intent(t, []string{"update"}, buckets(base)...)
		out, _ := l.run(t, "plan")
		if !strings.Contains(out, "does not re-create") {
			t.Errorf("plan with enable_on_create on an existing bucket:\n%s", out)
		}
		// PutObjectLockConfiguration で有効にする（承認つき）。endpoint が既にある
		// バケットに有効にさせなければ、理由つきで止まる。
		base["sk-lock-plain"] = map[string]string{"object_lock": "{}"}
		l.intent(t, []string{"update"}, buckets(base)...)
		out, _ = l.run(t, "reconcile")
		t.Logf("turning Object Lock on for an existing bucket:\n%s", out)
		for _, ep := range l.servers {
			ol := lockOf(t, ep, "sk-lock-plain")
			if !ol.Enabled && !strings.Contains(out, "refused to turn Object Lock on") {
				t.Errorf("%s: neither turned on nor refused with the reason:\n%s", ep, out)
			}
			t.Logf("%s: an existing bucket after the approved enable: %+v", ep, ol)
		}
	})

	t.Run("the observe user reads with GetBucketObjectLockConfiguration", func(t *testing.T) {
		need(t, "minio")
		minioUser(t, "sakutei-lock-ro", "sakutei-lock-ro-secret-0123", append(append([]string{}, readActions...), "s3:GetBucketObjectLockConfiguration"))
		minioUser(t, "sakutei-lock-nolock", "sakutei-lock-nolock-secret-0123", readActions)
		user := func(ak, sk string) *s3bucket.Session {
			c, err := s3bucket.NewClient(s3bucket.ClientConfig{Endpoint: lab.servers["minio"].url, Timeout: 5 * time.Second,
				Credentials: func(context.Context) (s3bucket.Credentials, error) {
					return s3bucket.Credentials{AccessKey: ak, SecretKey: sk}, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := c.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
		ro := user("sakutei-lock-ro", "sakutei-lock-ro-secret-0123")
		if ol, err := ro.GetObjectLock(ctx, "sk-lock-years"); err != nil || !ol.Enabled {
			t.Errorf("Get with the observe user: %+v, %v", ol, err)
		}
		err := ro.PutObjectLock(ctx, "sk-lock-years", s3bucket.ObjectLock{Mode: "COMPLIANCE", Years: 1})
		if s3bucket.KindOf(err) != s3bucket.KindAuth {
			t.Errorf("Put with the observe user: %v; want 403", err)
		}
		if _, err := user("sakutei-lock-nolock", "sakutei-lock-nolock-secret-0123").GetObjectLock(ctx, "sk-lock-years"); s3bucket.KindOf(err) != s3bucket.KindAuth {
			t.Errorf("Get without s3:GetBucketObjectLockConfiguration: %v; want 403", err)
		}
	})

	t.Run("clean up", func(t *testing.T) {
		for _, ep := range l.servers {
			for _, b := range []string{"sk-lock-vault", "sk-lock-years", "sk-lock-plain"} {
				if err := session(t, ep).DeleteBucket(ctx, b); err != nil {
					t.Errorf("%s: DeleteBucket %s: %v", ep, b, err)
				}
			}
		}
	})
}
