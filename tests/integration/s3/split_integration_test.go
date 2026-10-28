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

// 観測用と書き込み用のキー（§20.7、#186）を MinIO の利用者とポリシーで確かめる。
// 読むだけのポリシー（readActions）の利用者を observe:、書き込みのポリシー
// （writeActions）の利用者を write: に書き、観測はそれで通り、書き込みは
// 読むだけの利用者では断られ、書き込み用の利用者で収束することを見る。
// readActions の 1 つでも欠ければ観測できない（Unknown。absent にしない）ことも
// 1 つずつ確かめる（それぞれが要る最小の組であること）。
//
// 利用者とポリシーは MinIO のコンテナの中の mc（mc admin）で作る。

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

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// readActions は provider の観測（ListBuckets と 4 つの Get）が要る許可の全部。
var readActions = []string{
	"s3:ListAllMyBuckets",
	"s3:GetBucketVersioning",
	"s3:GetBucketPolicy",
	"s3:GetLifecycleConfiguration",
	"s3:GetBucketTagging",
}

// writeActions は書き込み（作る・消す・4 つの設定の Put と外す、Apply の中の
// 読み戻しの Get）が要る許可。
var writeActions = []string{
	"s3:ListAllMyBuckets",
	"s3:CreateBucket",
	"s3:DeleteBucket",
	"s3:PutBucketVersioning",
	"s3:GetBucketVersioning",
	"s3:PutBucketPolicy",
	"s3:DeleteBucketPolicy",
	"s3:GetBucketPolicy",
	"s3:PutLifecycleConfiguration",
	"s3:GetLifecycleConfiguration",
	"s3:PutBucketTagging",
	"s3:GetBucketTagging",
}

func policyDoc(actions []string) string {
	b, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Action": actions, "Resource": []string{"arn:aws:s3:::*"},
	}}})
	return string(b)
}

// mcAdmin は MinIO のコンテナの中で mc を root の資格で動かす。
func mcAdmin(t *testing.T, script string) {
	t.Helper()
	out, err := docker("exec", lab.servers["minio"].container, "sh", "-c",
		"mc alias set local http://127.0.0.1:9000 "+accessKey+" "+secretKey+" >/dev/null && "+script)
	if err != nil {
		t.Fatalf("mc: %v\n%s", err, out)
	}
}

// minioUser は policy の許可だけを持つ利用者を作る。
func minioUser(t *testing.T, user, secret string, actions []string) {
	t.Helper()
	doc := policyDoc(actions)
	if strings.Contains(doc, "'") {
		t.Fatal("the policy must not contain a single quote")
	}
	mcAdmin(t, fmt.Sprintf("printf '%%s' '%s' > /tmp/%s.json && mc admin policy create local %s /tmp/%s.json >/dev/null && "+
		"mc admin user add local %s %s >/dev/null && mc admin policy attach local %s --user %s >/dev/null",
		doc, user, user, user, user, secret, user, user))
}

type splitLab struct {
	dir, cfg, store string
}

func (l splitLab) writeStore(t *testing.T, withWrite bool) {
	t.Helper()
	secrets := map[string]any{
		"s3/ro_access_key": map[string]any{"value": "sakutei-ro"},
		"s3/ro_secret_key": map[string]any{"value": "sakutei-ro-secret-0123"},
	}
	if withWrite {
		secrets["s3/rw_access_key"] = map[string]any{"value": "sakutei-rw"}
		secrets["s3/rw_secret_key"] = map[string]any{"value": "sakutei-rw-secret-0123"}
	}
	b, _ := json.Marshal(map[string]any{"secrets": secrets})
	if err := os.WriteFile(l.store, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (l splitLab) intent(t *testing.T, rs ...resource) {
	t.Helper()
	b, _ := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err := os.WriteFile(filepath.Join(l.dir, "intent", "buckets.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (l splitLab) run(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{verb,
		"--intent", filepath.Join(l.dir, "intent"),
		"--state", filepath.Join(l.dir, "fake-state.json"),
		"--cause-state", filepath.Join(l.dir, "cause-state.json"),
		"--s3-config", l.cfg,
	}, extra...)
	cmd := exec.Command(lab.bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out) + stderr.String(), ee.ExitCode()
		}
		t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out) + stderr.String(), 0
}

func (l splitLab) status(t *testing.T) statusDoc {
	t.Helper()
	out, _ := l.run(t, "status", "--json")
	var doc statusDoc
	// --json は標準出力の最初の JSON。標準エラーの文言はその後ろ。
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	return doc
}

// observeOnly は access_key・secret_key（読むだけの利用者）だけの endpoint の
// Provider（書かない回）。
func observeOnly(t *testing.T, dir, user, secret string) *s3bucket.Provider {
	t.Helper()
	store := filepath.Join(dir, user+"-secrets.json")
	b, _ := json.Marshal(map[string]any{"secrets": map[string]any{
		"s3/ak": map[string]any{"value": user}, "s3/sk": map[string]any{"value": secret},
	}})
	if err := os.WriteFile(store, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, user+".yaml")
	body := fmt.Sprintf("secret_store: %s\nendpoints:\n  minio:\n    endpoint: %s\n    observe:\n      access_key: secretref:file/s3/ak\n      secret_key: secretref:file/s3/sk\n"+
		"    write:\n      access_key: secretref:file/s3/missing_ak\n      secret_key: secretref:file/s3/missing_sk\n    bucket_prefix: sk-split-\n    timeout: 5s\n",
		store, lab.servers["minio"].url)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := loader.ReadTree(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s3bucket.NewFromConfigTreeWith(cfg, tree, s3bucket.LoadOptions{ObserveOnly: true})
	if err != nil {
		t.Fatalf("an observe-only run without the write keys: %v", err)
	}
	return p
}

func TestS3CredentialSplit(t *testing.T) {
	need(t, "minio")
	ctx := context.Background()
	l := splitLab{dir: filepath.Join(lab.dir, "split")}
	if err := os.MkdirAll(filepath.Join(l.dir, "intent"), 0o755); err != nil {
		t.Fatal(err)
	}
	l.cfg = filepath.Join(l.dir, "s3.yaml")
	l.store = filepath.Join(l.dir, "secrets.json")
	cfg := fmt.Sprintf("secret_store: secrets.json\nendpoints:\n  minio:\n    endpoint: %s\n"+
		"    observe:\n      access_key: secretref:file/s3/ro_access_key\n      secret_key: secretref:file/s3/ro_secret_key\n"+
		"    write:\n      access_key: secretref:file/s3/rw_access_key\n      secret_key: secretref:file/s3/rw_secret_key\n"+
		"    bucket_prefix: sk-split-\n    timeout: 5s\n", lab.servers["minio"].url)
	if err := os.WriteFile(l.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	minioUser(t, "sakutei-ro", "sakutei-ro-secret-0123", readActions)
	minioUser(t, "sakutei-rw", "sakutei-rw-secret-0123", writeActions)
	t.Cleanup(func() {
		sess, err := lab.clients["minio"].Open(ctx)
		if err != nil {
			return
		}
		for _, b := range []string{"sk-split-app", "sk-split-orphan"} {
			_ = sess.DeleteBucket(ctx, b)
		}
	})

	app := resource{Provider: s3bucket.Name, ID: "minio/sk-split-app", DeletionPolicy: "PruneWithinScope", Fields: map[string]string{
		"versioning": "Enabled",
		"policy":     policyFor("sk-split-app"),
		"lifecycle":  lifecycle,
		"tags":       `{"env": "integration", "owner": "sakutei"}`,
	}}
	l.intent(t, app)

	t.Run("observe with the read-only user, without the write keys", func(t *testing.T) {
		l.writeStore(t, false)
		s := l.status(t)
		if !s.Complete || s.Summary.Drift != 1 {
			t.Fatalf("status with the read-only user only: %+v, want complete and 1 drifted (not created yet)", s)
		}
		out, code := l.run(t, "plan")
		if code != 0 && code != 2 {
			t.Fatalf("plan with the read-only user only exited %d:\n%s", code, out)
		}
		out, code = l.run(t, "reconcile")
		if code == 0 || !strings.Contains(out, "endpoints.minio.write.access_key") {
			t.Fatalf("reconcile without the write keys exited %d; want a refusal naming endpoints.minio.write.access_key:\n%s", code, out)
		}
		if hasBucket(t, "minio", "sk-split-app") {
			t.Fatal("the bucket was created without the write keys")
		}
	})

	t.Run("a write with the read-only user is refused", func(t *testing.T) {
		ro, err := s3bucket.NewClient(s3bucket.ClientConfig{Endpoint: lab.servers["minio"].url, Timeout: 5 * time.Second,
			Credentials: func(context.Context) (s3bucket.Credentials, error) {
				return s3bucket.Credentials{AccessKey: "sakutei-ro", SecretKey: "sakutei-ro-secret-0123"}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		sess, err := ro.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = sess.CreateBucket(ctx, "sk-split-app")
		if s3bucket.KindOf(err) != s3bucket.KindAuth || s3bucket.CodeOf(err) != "AccessDenied" {
			t.Fatalf("CreateBucket with the read-only user = %v; want 403 AccessDenied", err)
		}
		if err := session(t, "minio").CreateBucket(ctx, "sk-split-orphan"); err != nil { // root で作る。
			t.Fatal(err)
		}
		for name, op := range map[string]func() error{
			"PutBucketTagging":    func() error { return sess.PutTagging(ctx, "sk-split-orphan", map[string]string{"a": "b"}) },
			"PutBucketVersioning": func() error { return sess.PutVersioning(ctx, "sk-split-orphan", "Enabled") },
			"DeleteBucket":        func() error { return sess.DeleteBucket(ctx, "sk-split-orphan") },
		} {
			if err := op(); s3bucket.KindOf(err) != s3bucket.KindAuth {
				t.Errorf("%s with the read-only user = %v; want 403", name, err)
			}
		}
		if !hasBucket(t, "minio", "sk-split-orphan") {
			t.Fatal("the read-only user deleted a bucket")
		}
	})

	t.Run("converge with the write user", func(t *testing.T) {
		l.writeStore(t, true)
		out, code := l.run(t, "reconcile")
		if code != 0 {
			t.Fatalf("reconcile with the write user exited %d:\n%s", code, out)
		}
		if hasBucket(t, "minio", "sk-split-orphan") {
			t.Errorf("the empty orphan was not pruned with the write user:\n%s", out)
		}
		if s := l.status(t); !s.Complete || s.Summary.Conformant != 1 || s.Summary.Drift != 0 {
			t.Fatalf("status after reconcile: %+v\n%s", s, out)
		}
		// 外す（DeleteBucketPolicy・DeleteBucketLifecycle・DeleteBucketTagging）も
		// 書き込み用の利用者で。
		cleared := app
		cleared.Fields = map[string]string{"versioning": "Suspended", "policy": "", "lifecycle": "", "tags": ""}
		l.intent(t, cleared)
		if out, code := l.run(t, "reconcile"); code != 0 {
			t.Fatalf("reconcile removing the settings exited %d:\n%s", code, out)
		}
		if s := l.status(t); s.Summary.Conformant != 1 || s.Summary.Drift != 0 {
			t.Fatalf("status after removing the settings: %+v", s)
		}
		l.intent(t, app)
		if out, code := l.run(t, "reconcile"); code != 0 {
			t.Fatalf("reconcile setting them again exited %d:\n%s", code, out)
		}
		if s := l.status(t); s.Summary.Conformant != 1 {
			t.Fatalf("status after setting them again: %+v", s)
		}
	})

	t.Run("each read action is needed and a missing one is unknown, not absent", func(t *testing.T) {
		dir := filepath.Join(l.dir, "min")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		full := observeOnly(t, dir, "sakutei-ro", "sakutei-ro-secret-0123")
		obs, err := full.Read(ctx, "minio/sk-split-app")
		if err != nil || obs.Availability != core.AvailabilityOk || obs.State == nil {
			t.Fatalf("Read with the read-only policy = %+v, %v; want observed", obs, err)
		}
		if _, err := full.List(ctx, core.NewScope(nil)); err != nil {
			t.Fatalf("List with the read-only policy: %v", err)
		}
		for i, missing := range readActions {
			var actions []string
			for _, a := range readActions {
				if a != missing {
					actions = append(actions, a)
				}
			}
			user := fmt.Sprintf("sakutei-ro-%d", i)
			minioUser(t, user, user+"-secret-0123", actions)
			p := observeOnly(t, dir, user, user+"-secret-0123")
			obs, err := p.Read(ctx, "minio/sk-split-app")
			if err == nil || obs.Availability != core.AvailabilityUnknown || obs.State != nil {
				t.Errorf("without %s: Read = %+v, %v; want unknown", missing, obs, err)
				continue
			}
			t.Logf("without %s: %v", missing, err)
		}
	})
}
