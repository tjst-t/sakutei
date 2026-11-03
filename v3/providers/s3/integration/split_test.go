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

//go:build integration_v3_s3

package integration

// split_test.go — 観測用と書き込み用の資格（E-G5）を MinIO の利用者とポリシーで
// 確かめる（v2 の split_integration_test.go を移した）。読むだけのポリシー
// （readActions）の利用者を observe:、書き込みのポリシー（writeActions）の利用者を
// write: に書き、観測はそれで通り、書き込み用の secret key のファイルが無くても plan は
// 回り、書き込みは読むだけの利用者では断られ、書き込み用の利用者で収束することを
// 見る。readActions の 1 つでも欠ければ観測できない（Unknown。無いと読まない）ことも
// 1 つずつ確かめる。利用者とポリシーは MinIO のコンテナの中の mc（mc admin）で作る。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/s3"
)

// readActions は provider の観測（ListBuckets と 4 つの Get）が要る許可の全部。
var readActions = []string{
	"s3:ListAllMyBuckets",
	"s3:GetBucketVersioning",
	"s3:GetBucketPolicy",
	"s3:GetLifecycleConfiguration",
	"s3:GetBucketTagging",
}

// writeActions は書き込み（作る・消す・4 つの設定の Put と外す、Apply の中の読み直しの
// Get）が要る許可。
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

// mcAdmin は MinIO のコンテナの中で mc を管理者の資格で動かす。
func mcAdmin(t *testing.T, script string) {
	t.Helper()
	out, err := docker("exec", lab.servers["minio"].container, "sh", "-c",
		"mc alias set local http://127.0.0.1:9000 "+accessKey+" "+secretKey+" >/dev/null && "+script)
	if err != nil {
		t.Fatalf("mc: %v\n%s", err, out)
	}
}

// minioUser は actions の許可だけを持つ利用者を作り、secret key のファイルを返す。
func minioUser(t *testing.T, user, secret string, actions []string) string {
	t.Helper()
	doc := policyDoc(actions)
	if strings.Contains(doc, "'") {
		t.Fatal("ポリシーに ' を入れない")
	}
	mcAdmin(t, fmt.Sprintf("printf '%%s' '%s' > /tmp/%s.json && mc admin policy create local %s /tmp/%s.json >/dev/null && "+
		"mc admin user add local %s %s >/dev/null && mc admin policy attach local %s --user %s >/dev/null",
		doc, user, user, user, user, secret, user, user))
	f := filepath.Join(lab.dir, user+".secret")
	if err := os.WriteFile(f, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// observer は user の資格だけの endpoint minio の provider。
func observer(t *testing.T, user, secretFile string) core.Provider {
	t.Helper()
	p, err := s3.Module(map[string]any{"endpoints": map[string]any{"minio": map[string]any{
		"endpoint": lab.servers["minio"].url, "timeout": "5s",
		"observe": map[string]any{"access_key": user, "secret_key_file": secretFile},
		"write":   map[string]any{"access_key": "nobody", "secret_key_file": filepath.Join(lab.dir, "missing.secret")},
	}}})
	if err != nil {
		t.Fatalf("書き込み用の secret key のファイルが無くても作れる: %v", err)
	}
	return p
}

func TestS3CredentialSplit(t *testing.T) {
	need(t, "minio")
	ctx := context.Background()
	roFile := minioUser(t, "sakutei-ro", "sakutei-ro-secret-0123", readActions)
	minioUser(t, "sakutei-rw", "sakutei-rw-secret-0123", writeActions)
	rwFile := filepath.Join(lab.dir, "split-rw.secret") // 初めは無い
	w := newWS(t, "split", endpointCfg{name: "minio", server: "minio",
		observe: [2]string{"sakutei-ro", roFile}, write: [2]string{"sakutei-rw", rwFile}})
	t.Cleanup(func() {
		sess, err := lab.clients["minio"].Open(ctx)
		if err != nil {
			return
		}
		for _, b := range []string{"sk-split-app", "sk-split-orphan"} {
			_ = sess.DeleteBucket(ctx, b)
		}
	})
	app := bucketRes("minio/sk-split-app", map[string]any{
		"versioning": "Enabled", "policy": policyFor("sk-split-app"), "lifecycle": lifecycle(),
		"tags": map[string]any{"env": "integration", "owner": "sakutei"},
	})
	ranges := []rangeDecl{rng("minio", "sk-split-", true)}
	w.intent(t, ranges, app)

	t.Run("読むだけの利用者で観測し、書き込み用の資格が無ければ書かない", func(t *testing.T) {
		r, code, errOut := w.run(t, "plan")
		if code != cli.ExitOK {
			t.Fatalf("plan exit %d: %s", code, errOut)
		}
		wantOps(t, "plan", r, map[string]plan.Op{"s3/bucket/minio/sk-split-app": plan.Create})
		r, code, _ = w.run(t, "reconcile")
		c := changeOf(r, "s3/bucket/minio/sk-split-app")
		if code == cli.ExitOK || c.Outcome != plan.OutcomeError || !strings.Contains(c.Error, "secret_key_file") {
			t.Fatalf("書き込み用の資格の無い reconcile: exit %d %+v", code, c)
		}
		if hasBucket(t, "minio", "sk-split-app") {
			t.Fatal("書き込み用の資格が無いのにバケットを作った")
		}
	})

	t.Run("読むだけの利用者の書き込みは断られる", func(t *testing.T) {
		ro, err := s3.NewClient(s3.ClientConfig{Endpoint: lab.servers["minio"].url, Timeout: 5 * time.Second, Credentials: creds("sakutei-ro", "sakutei-ro-secret-0123")})
		if err != nil {
			t.Fatal(err)
		}
		sess, err := ro.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.CreateBucket(ctx, "sk-split-app", false); s3.KindOf(err) != s3.KindAuth || s3.CodeOf(err) != "AccessDenied" {
			t.Fatalf("読むだけの利用者の CreateBucket = %v（want 403 AccessDenied）", err)
		}
		if err := session(t, "minio").CreateBucket(ctx, "sk-split-orphan", false); err != nil { // 管理者で作る。
			t.Fatal(err)
		}
		for name, op := range map[string]func() error{
			"PutBucketTagging":    func() error { return sess.PutTagging(ctx, "sk-split-orphan", map[string]string{"a": "b"}) },
			"PutBucketVersioning": func() error { return sess.PutVersioning(ctx, "sk-split-orphan", "Enabled") },
			"DeleteBucket":        func() error { return sess.DeleteBucket(ctx, "sk-split-orphan") },
		} {
			if err := op(); s3.KindOf(err) != s3.KindAuth {
				t.Errorf("読むだけの利用者の %s = %v（want 403）", name, err)
			}
		}
		if !hasBucket(t, "minio", "sk-split-orphan") {
			t.Fatal("読むだけの利用者がバケットを消した")
		}
	})

	t.Run("書き込み用の利用者で収束する", func(t *testing.T) {
		if err := os.WriteFile(rwFile, []byte("sakutei-rw-secret-0123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r, code, errOut := w.run(t, "reconcile")
		if code != cli.ExitOK {
			t.Fatalf("reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		// 宣言の外のバケットは prune: true でも消さずに見せる（中身を失う種類）。
		if c := changeOf(r, "s3/bucket/minio/sk-split-orphan"); c.Op != plan.Unmanaged || !hasBucket(t, "minio", "sk-split-orphan") {
			t.Errorf("宣言の外の sk-split-orphan: %s", c.Op)
		}
		if r, _, _ = w.run(t, "plan"); !converged(r) {
			t.Fatalf("収束したあと:\n%s", dump(r))
		}
		// 外す（DeleteBucketPolicy・DeleteBucketLifecycle・DeleteBucketTagging）も書き込み用の利用者で。
		cleared := bucketRes("minio/sk-split-app", map[string]any{"versioning": "Suspended", "policy": map[string]any{}, "lifecycle": []any{}, "tags": map[string]any{}})
		w.intent(t, ranges, cleared)
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK || !converged(r) {
			t.Fatalf("外す reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		w.intent(t, ranges, app)
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK || !converged(r) {
			t.Fatalf("置き直す reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
	})

	t.Run("観測の許可はどれも要り、欠ければ Unknown（無いと読まない）", func(t *testing.T) {
		id := core.ID{Type: "s3/bucket", Name: "minio/sk-split-app"}
		full := observer(t, "sakutei-ro", roFile)
		obs, err := full.Observe(ctx, []core.ID{id})
		if err != nil || obs[id].Availability != core.Present {
			t.Fatalf("読むだけのポリシーの観測 = %+v, %v", obs[id], err)
		}
		if l, err := full.List(ctx, core.Range{Provider: "s3", Args: map[string]string{"endpoint": "minio", "prefix": "sk-split-"}}); err != nil || !l.Complete {
			t.Fatalf("読むだけのポリシーの List: %+v %v", l, err)
		}
		for i, missing := range readActions {
			var actions []string
			for _, a := range readActions {
				if a != missing {
					actions = append(actions, a)
				}
			}
			user := fmt.Sprintf("sakutei-ro-%d", i)
			f := minioUser(t, user, user+"-secret-0123", actions)
			obs, err := observer(t, user, f).Observe(ctx, []core.ID{id})
			if err != nil || obs[id].Availability != core.Unknown || obs[id].Fields != nil {
				t.Errorf("%s が無い: %+v, %v（want unknown）", missing, obs[id], err)
				continue
			}
			t.Logf("%s が無い: %s", missing, obs[id].Note)
		}
	})
}
