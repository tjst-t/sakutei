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

// s3_test.go — v3 の sakutei の CLI（plan・reconcile）を本物の S3 互換のサーバに
// 向けて回す（v2 の tests/integration/s3/s3_integration_test.go の step を v3 に移した）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/s3"
)

func policyFor(bucket string) map[string]any {
	return map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Sid": "public-read", "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::" + bucket + "/*",
	}}}
}

func lifecycle() []any {
	return []any{
		map[string]any{"ID": "expire-logs", "Status": "Enabled", "Filter": map[string]any{"Prefix": "logs/"}, "Expiration": map[string]any{"Days": 30}},
		map[string]any{"ID": "noncurrent", "Status": "Enabled", "Filter": map[string]any{}, "NoncurrentVersionExpiration": map[string]any{"NoncurrentDays": 7}},
	}
}

// appBucket は全部の設定を宣言したバケット。
func appBucket(ep string) resource {
	return bucketRes(ep+"/sk-it-app", map[string]any{
		"versioning": "Enabled",
		"policy":     policyFor("sk-it-app"),
		"lifecycle":  lifecycle(),
		"tags":       map[string]any{"env": "integration", "owner": "sakutei"},
	})
}

func TestS3(t *testing.T) {
	ctx := context.Background()
	main := mainServers()
	for name, why := range lab.skipped {
		t.Logf("%s（任意）は使わない: %s", name, why)
	}
	var eps []endpointCfg
	for _, n := range []string{"seaweedfs", "minio", "seaweedfs-old"} {
		if _, ok := lab.servers[n]; ok {
			eps = append(eps, endpointCfg{name: n, server: n})
		}
	}
	// S3 互換の実装との違いへの対応：同じサーバを別の名前・別の接頭辞（sk-it- と重ならない）
	// で、扱う設定を絞って足す。
	eps = append(eps, endpointCfg{name: "seaweedfs-compat", server: "seaweedfs", fields: []string{"policy", "tags"}, aliases: map[string]string{"VENDOR_COLD": "GLACIER"}})
	w := newWS(t, "main", eps...)
	ranges := func(prune bool) []rangeDecl {
		var rs []rangeDecl
		for _, e := range eps {
			p := "sk-it-"
			if e.name == "seaweedfs-compat" {
				p = "sk-compat-"
			}
			rs = append(rs, rng(e.name, p, prune))
		}
		return rs
	}
	apps := func(more ...resource) []resource {
		var rs []resource
		for _, ep := range main {
			rs = append(rs, appBucket(ep))
		}
		return append(rs, more...)
	}
	w.intent(t, ranges(false), apps()...)

	t.Run("空から作る", func(t *testing.T) {
		r, code, errOut := w.run(t, "plan")
		if code != cli.ExitOK {
			t.Fatalf("plan exit %d: %s", code, errOut)
		}
		for _, ep := range main {
			wantOps(t, "作る前", r, map[string]plan.Op{"s3/bucket/" + ep + "/sk-it-app": plan.Create})
		}
		if r, code, errOut = w.run(t, "reconcile"); code != cli.ExitOK {
			t.Fatalf("reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		if r, _, _ = w.run(t, "plan"); !converged(r) {
			t.Fatalf("作ったあと:\n%s", dump(r))
		}
		for _, ep := range main {
			sess := session(t, ep)
			if v, err := sess.GetVersioning(ctx, "sk-it-app"); err != nil || v != "Enabled" {
				t.Errorf("%s: versioning = %q, %v", ep, v, err)
			}
			pol, err := sess.GetPolicy(ctx, "sk-it-app")
			want, _ := s3.CanonPolicy(`{"Version":"2012-10-17","Statement":[{"Sid":"public-read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::sk-it-app/*"}]}`)
			if got, _ := s3.CanonPolicy(pol); err != nil || got != want {
				t.Errorf("%s: policy = %s, %v（want %s）", ep, pol, err, want)
			}
			t.Logf("%s はポリシーを %s で持つ", ep, strings.TrimSpace(pol))
		}
	})

	t.Run("手で変えた policy と versioning はずれで、戻る", func(t *testing.T) {
		for _, ep := range main {
			sess := session(t, ep)
			if err := sess.PutVersioning(ctx, "sk-it-app", "Suspended"); err != nil {
				t.Fatal(err)
			}
			if err := sess.PutPolicy(ctx, "sk-it-app", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::sk-it-app/*"}]}`); err != nil {
				t.Fatal(err)
			}
		}
		r, _, _ := w.run(t, "plan")
		for _, ep := range main {
			c := changeOf(r, "s3/bucket/"+ep+"/sk-it-app")
			if c.Op != plan.Update || len(c.Fields) != 2 {
				t.Fatalf("%s: 手動の変更のあと %s %+v", ep, c.Op, c.Fields)
			}
		}
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK {
			t.Fatalf("reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		if r, _, _ := w.run(t, "plan"); !converged(r) {
			t.Fatalf("戻したあと:\n%s", dump(r))
		}
		for _, ep := range main {
			if v, _ := session(t, ep).GetVersioning(ctx, "sk-it-app"); v != "Enabled" {
				t.Errorf("%s: versioning = %q", ep, v)
			}
		}
	})

	t.Run("何 cycle 回しても宣言どおり", func(t *testing.T) {
		before := map[string]string{}
		for _, ep := range main {
			before[ep], _ = session(t, ep).GetPolicy(ctx, "sk-it-app")
		}
		for i := 0; i < 3; i++ {
			r, code, errOut := w.run(t, "reconcile")
			if code != cli.ExitOK || !converged(r) {
				t.Fatalf("cycle %d exit %d: %s\n%s", i, code, errOut, dump(r))
			}
		}
		for _, ep := range main {
			if after, _ := session(t, ep).GetPolicy(ctx, "sk-it-app"); after != before[ep] {
				t.Errorf("%s: cycle を重ねてポリシーが書き換わった:\n%s\n%s", ep, before[ep], after)
			}
		}
	})

	t.Run("宣言の外のバケットは消さずに見せる", func(t *testing.T) {
		for _, ep := range main {
			sess := session(t, ep)
			for _, b := range []string{"sk-it-orphan-full", "sk-it-orphan-empty", "other-not-managed"} {
				if err := sess.CreateBucket(ctx, b, false); err != nil {
					t.Fatal(err)
				}
			}
			putObject(t, ep, "sk-it-orphan-full", "data/object.txt")
		}
		w.intent(t, ranges(true), apps()...)
		r, code, errOut := w.run(t, "reconcile")
		if code != cli.ExitOK {
			t.Fatalf("reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		for _, ep := range main {
			for _, b := range []string{"sk-it-orphan-full", "sk-it-orphan-empty"} {
				if c := changeOf(r, "s3/bucket/"+ep+"/"+b); c.Op != plan.Unmanaged || !hasBucket(t, ep, b) {
					t.Errorf("%s/%s: %s %q（want 宣言に無いものを消さない）", ep, b, c.Op, c.Note)
				}
			}
			if c := changeOf(r, "s3/bucket/"+ep+"/other-not-managed"); c.Op != "" || !hasBucket(t, ep, "other-not-managed") {
				t.Errorf("%s: Range の外のバケットが %s", ep, c.Op)
			}
		}
	})

	t.Run("ensure: absent で名指しして消す。中身のあるバケットは拒否する", func(t *testing.T) {
		var more []resource
		for _, ep := range main {
			more = append(more, resource{Type: "s3/bucket", ID: ep + "/sk-it-orphan-empty", Ensure: "absent"},
				resource{Type: "s3/bucket", ID: ep + "/sk-it-orphan-full", Ensure: "absent"})
		}
		w.intent(t, ranges(true), apps(more...)...)
		r, code, errOut := w.run(t, "reconcile")
		t.Logf("reconcile exit %d", code)
		for _, ep := range main {
			if c := changeOf(r, "s3/bucket/"+ep+"/sk-it-orphan-empty"); c.Outcome != plan.OutcomeOK || hasBucket(t, ep, "sk-it-orphan-empty") {
				t.Errorf("%s: 空のバケット %s %s %s\n%s", ep, c.Op, c.Outcome, c.Error, errOut)
			}
			c := changeOf(r, "s3/bucket/"+ep+"/sk-it-orphan-full")
			if c.Outcome != plan.OutcomeRefused || !strings.Contains(c.Error, "オブジェクトがある") || !hasBucket(t, ep, "sk-it-orphan-full") {
				t.Errorf("%s: 中身のあるバケット %s %s %q", ep, c.Op, c.Outcome, c.Error)
			}
		}
		// 中身を出してから消す（片づけ。オブジェクトは Sakutei の外で消す）。
		for _, ep := range main {
			deleteObject(t, ep, "sk-it-orphan-full", "data/object.txt")
		}
		if r, code, errOut = w.run(t, "reconcile"); code != cli.ExitOK {
			t.Fatalf("空にしたあとの reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		for _, ep := range main {
			if hasBucket(t, ep, "sk-it-orphan-full") {
				t.Errorf("%s: 空にしたバケットが残る", ep)
			}
		}
		w.intent(t, ranges(false), apps()...)
	})

	// 止めるサーバ（paused）と、動いたままのサーバ（running）。minio が使えれば minio を
	// 止めて seaweedfs を残す。使えなければ seaweedfs を止める。
	paused, running := "minio", "seaweedfs"
	if _, ok := lab.servers["minio"]; !ok {
		paused, running = "seaweedfs", ""
	}

	t.Run("止まったサーバは Unknown で何もしない", func(t *testing.T) {
		if out, err := docker("pause", lab.servers[paused].container); err != nil {
			t.Fatalf("docker pause: %v: %s", err, out)
		}
		defer func() {
			if out, err := docker("unpause", lab.servers[paused].container); err != nil {
				t.Fatalf("docker unpause: %v: %s", err, out)
			}
		}()
		w.intent(t, ranges(true), apps(bucketRes(paused+"/sk-it-new", nil))...)
		r, code, _ := w.run(t, "reconcile")
		if code != cli.ExitRemaining {
			t.Errorf("reconcile exit %d（want %d）", code, cli.ExitRemaining)
		}
		for _, id := range []string{paused + "/sk-it-app", paused + "/sk-it-new"} {
			if c := changeOf(r, "s3/bucket/"+id); c.Op != plan.Unknown {
				t.Errorf("%s: %s（want unknown）%s", id, c.Op, c.Note)
			}
		}
		for _, rs := range r.Ranges {
			if strings.Contains(rs.Range, "endpoint="+paused+",") && rs.Complete {
				t.Errorf("止まった %s の Range が完全: %+v", paused, rs)
			}
		}
		if running != "" {
			if c := changeOf(r, "s3/bucket/"+running+"/sk-it-app"); c.Op != plan.None {
				t.Errorf("動いている %s が %s", running, c.Op)
			}
		}
	})

	t.Run("戻れば再び収束する", func(t *testing.T) {
		if err := waitReady(paused, time.Minute); err != nil {
			t.Fatal(err)
		}
		w.intent(t, ranges(false), apps()...)
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK || !converged(r) {
			t.Fatalf("exit %d: %s\n%s", code, errOut, dump(r))
		}
	})

	t.Run("API を持たない endpoint", func(t *testing.T) {
		need(t, "seaweedfs-old")
		// 古い seaweedfs はバージョニング・ポリシー・タグの GET に既定の値を返すが、PUT は
		// 501 NotImplemented。宣言すればずれになり、Apply が分かる文で止まる（宣言しなければ
		// 比べない）。
		old := bucketRes("seaweedfs-old/sk-it-old", map[string]any{"versioning": "Enabled", "tags": map[string]any{"env": "integration"}})
		w.intent(t, ranges(false), apps(old)...)
		r, code, _ := w.run(t, "reconcile")
		c := changeOf(r, "s3/bucket/seaweedfs-old/sk-it-old")
		t.Logf("seaweedfs-old: exit %d %s %s %q", code, c.Op, c.Outcome, c.Error)
		if c.Outcome != plan.OutcomeError || !strings.Contains(c.Error, "NotImplemented") {
			t.Errorf("API を持たないと言わない: %+v", c)
		}
		if !hasBucket(t, "seaweedfs-old", "sk-it-old") {
			t.Error("バケット自体が作られていない")
		}
		w.intent(t, ranges(false), apps(bucketRes("seaweedfs-old/sk-it-old", nil))...)
		if r, _, _ := w.run(t, "plan"); changeOf(r, "s3/bucket/seaweedfs-old/sk-it-old").Op != plan.None {
			t.Errorf("宣言しない設定は比べない:\n%s", dump(r))
		}
	})

	t.Run("endpoint の fields に無い設定は読まない", func(t *testing.T) {
		// seaweedfs-compat の fields は policy・tags だけ。versioning・lifecycle は読まず、
		// 宣言しなければ宣言どおり、宣言すれば読み込み時に「扱わない」とエラーにする。
		compat := bucketRes("seaweedfs-compat/sk-compat-app", map[string]any{"policy": policyFor("sk-compat-app"), "tags": map[string]any{"env": "integration"}})
		w.intent(t, ranges(false), apps(compat)...)
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK || !converged(r) {
			t.Fatalf("exit %d: %s\n%s", code, errOut, dump(r))
		}
		r, _, _ := w.run(t, "plan")
		if c := changeOf(r, "s3/bucket/seaweedfs-compat/sk-compat-app"); c.Op != plan.None {
			t.Errorf("sk-compat-app: %s", c.Op)
		}
		if c := changeOf(r, "s3/bucket/seaweedfs/sk-compat-app"); c.Op != "" {
			t.Errorf("接頭辞の違う Range に sk-compat-app が出た: %s", c.Op)
		}
		if v, err := session(t, "seaweedfs").GetVersioning(ctx, "sk-compat-app"); err != nil || v == "Enabled" {
			t.Errorf("sk-compat-app の versioning = %q, %v（触らない）", v, err)
		}
		compat.Fields["versioning"] = "Enabled"
		w.intent(t, ranges(false), apps(compat)...)
		_, code, errOut := w.run(t, "plan")
		if code != cli.ExitRejected || !strings.Contains(errOut, "versioning を扱わない") || !strings.Contains(errOut, "fields") {
			t.Errorf("fields に無い versioning の宣言: exit %d\n%s", code, errOut)
		}
		w.intent(t, ranges(false), apps()...)
	})
}
