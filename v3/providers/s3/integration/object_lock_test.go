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

// object_lock_test.go — Object Lock（E-C1）を本物のサーバで確かめる（v2 の
// object_lock_integration_test.go を移した）。seaweedfs（必須）と、使えれば minio に、
// object_lock を fields に書いた endpoint（接頭辞 sk-lock-）を向け、作る・観測する・
// 保持の設定を変える（承認が要る。v3 の sakutei approve で承認する）を回す。minio
// では、観測用のユーザー（GetBucketObjectLockConfiguration だけ）で Get が通り Put が
// 拒否されることも確かめる。

import (
	"context"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/s3"
)

func lockOf(t *testing.T, server, bucket string) s3.ObjectLock {
	t.Helper()
	ol, err := session(t, server).GetObjectLock(context.Background(), bucket)
	if err != nil {
		t.Fatalf("%s: GetObjectLockConfiguration %s: %v", server, bucket, err)
	}
	return ol
}

func TestS3ObjectLock(t *testing.T) {
	ctx := context.Background()
	servers := mainServers()
	var eps []endpointCfg
	var ranges []rangeDecl
	for _, n := range servers {
		eps = append(eps, endpointCfg{name: n, server: n, fields: []string{"versioning", "policy", "lifecycle", "tags", "object_lock"}})
		ranges = append(ranges, rng(n, "sk-lock-", false))
	}
	w := newWS(t, "lock", eps...)
	buckets := func(fields map[string]map[string]any) []resource {
		var rs []resource
		for _, ep := range servers {
			for b, f := range fields {
				rs = append(rs, bucketRes(ep+"/"+b, f))
			}
		}
		return rs
	}
	gov1 := map[string]any{"object_lock": map[string]any{"enable_on_create": true, "mode": "GOVERNANCE", "days": 1}}
	years := map[string]any{"object_lock": map[string]any{"enable_on_create": true, "mode": "COMPLIANCE", "years": 1}}

	t.Run("Object Lock つきで作って観測する", func(t *testing.T) {
		w.intent(t, ranges, buckets(map[string]map[string]any{"sk-lock-vault": gov1, "sk-lock-years": years})...)
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK {
			t.Fatalf("reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		for _, ep := range servers {
			if ol := lockOf(t, ep, "sk-lock-vault"); !ol.Enabled || ol.Mode != "GOVERNANCE" || ol.Days != 1 {
				t.Errorf("%s: sk-lock-vault = %+v", ep, ol)
			}
			ol := lockOf(t, ep, "sk-lock-years")
			t.Logf("%s は Years 1 の保持を %+v で持つ", ep, ol)
			if !ol.Enabled || ol.Mode != "COMPLIANCE" || (ol.Years != 1 && ol.Days != 365) {
				t.Errorf("%s: sk-lock-years = %+v", ep, ol)
			}
			if v, err := session(t, ep).GetVersioning(ctx, "sk-lock-vault"); err != nil || v != "Enabled" {
				t.Logf("%s: Object Lock つきで作ったバケットの versioning = %q, %v", ep, v, err)
			}
		}
		for i := 0; i < 3; i++ {
			if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK || !converged(r) {
				t.Fatalf("cycle %d exit %d: %s\n%s", i, code, errOut, dump(r))
			}
		}
	})

	t.Run("保持の設定を変えるには承認が要る", func(t *testing.T) {
		gov2 := map[string]any{"object_lock": map[string]any{"enable_on_create": true, "mode": "GOVERNANCE", "days": 2}}
		w.intent(t, ranges, buckets(map[string]map[string]any{"sk-lock-vault": gov2, "sk-lock-years": years})...)
		r, code, _ := w.run(t, "reconcile")
		t.Logf("承認の前の reconcile exit %d", code)
		var requests []string
		for _, ep := range servers {
			c := changeOf(r, "s3/bucket/"+ep+"/sk-lock-vault")
			if c.Op != plan.Update || !c.NeedsApproval || !c.Destructive || c.Request == "" || c.Outcome != "" ||
				len(c.Classified) == 0 || !strings.Contains(c.Classified[0], "1 → 2 日に延ばす") {
				t.Fatalf("%s: 承認を求めない: %+v", ep, c)
			}
			requests = append(requests, c.Request)
			if ol := lockOf(t, ep, "sk-lock-vault"); ol.Days != 1 {
				t.Errorf("%s: 承認の前に変わった: %+v", ep, ol)
			}
		}
		for _, id := range requests {
			w.approve(t, id)
		}
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK {
			t.Fatalf("承認したあとの reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		for _, ep := range servers {
			if ol := lockOf(t, ep, "sk-lock-vault"); ol.Mode != "GOVERNANCE" || ol.Days != 2 {
				t.Errorf("%s: 承認したあと: %+v", ep, ol)
			}
		}
		// 既定の保持を外す（これも承認が要る）。
		none := map[string]any{"object_lock": map[string]any{"enable_on_create": true}}
		w.intent(t, ranges, buckets(map[string]map[string]any{"sk-lock-vault": none, "sk-lock-years": years})...)
		r, _, _ = w.run(t, "reconcile")
		for _, ep := range servers {
			c := changeOf(r, "s3/bucket/"+ep+"/sk-lock-vault")
			if !c.NeedsApproval || c.Request == "" || len(c.Classified) == 0 || !strings.Contains(c.Classified[0], "を外す") {
				t.Fatalf("%s: 外すのに承認を求めない: %+v", ep, c)
			}
			w.approve(t, c.Request)
		}
		if r, code, errOut := w.run(t, "reconcile"); code != cli.ExitOK || !converged(r) {
			t.Fatalf("外す reconcile exit %d: %s\n%s", code, errOut, dump(r))
		}
		for _, ep := range servers {
			if ol := lockOf(t, ep, "sk-lock-vault"); !ol.Enabled || ol.Mode != "" {
				t.Errorf("%s: 外したあと: %+v", ep, ol)
			}
		}
	})

	t.Run("Object Lock の無い既にあるバケット", func(t *testing.T) {
		for _, ep := range servers {
			if err := session(t, ep).CreateBucket(ctx, "sk-lock-plain", false); err != nil {
				t.Fatal(err)
			}
		}
		base := map[string]map[string]any{"sk-lock-vault": {"object_lock": map[string]any{}}, "sk-lock-years": years}
		// enable_on_create は作るときだけ：承認を求めず、作り直さずに理由つきで止まる。
		base["sk-lock-plain"] = map[string]any{"object_lock": map[string]any{"enable_on_create": true}}
		w.intent(t, ranges, buckets(base)...)
		r, _, _ := w.run(t, "reconcile")
		for _, ep := range servers {
			c := changeOf(r, "s3/bucket/"+ep+"/sk-lock-plain")
			if c.NeedsApproval || c.Outcome != plan.OutcomeError || !strings.Contains(c.Error, "作り直さない") {
				t.Errorf("%s: enable_on_create の食い違い: %+v", ep, c)
			}
			if lockOf(t, ep, "sk-lock-plain").Enabled {
				t.Errorf("%s: 拒否したのに Object Lock が有効", ep)
			}
		}
		// PutObjectLockConfiguration で有効にする（承認つき）。endpoint が既にあるバケットに
		// 有効にさせなければ、理由つきで止まる。
		base["sk-lock-plain"] = map[string]any{"object_lock": map[string]any{}}
		w.intent(t, ranges, buckets(base)...)
		r, _, _ = w.run(t, "reconcile")
		for _, ep := range servers {
			c := changeOf(r, "s3/bucket/"+ep+"/sk-lock-plain")
			if !c.NeedsApproval || c.Request == "" || len(c.Classified) == 0 || !strings.Contains(c.Classified[0], "有効にする") {
				t.Fatalf("%s: 既にあるバケットに掛けるのに承認を求めない: %+v", ep, c)
			}
			w.approve(t, c.Request)
		}
		r, code, _ := w.run(t, "reconcile")
		for _, ep := range servers {
			c := changeOf(r, "s3/bucket/"+ep+"/sk-lock-plain")
			ol := lockOf(t, ep, "sk-lock-plain")
			t.Logf("%s: 承認して既にあるバケットに掛けた: exit %d %s %q → %+v", ep, code, c.Outcome, c.Error, ol)
			if !ol.Enabled && !strings.Contains(c.Error, "有効化を拒否した") {
				t.Errorf("%s: 有効にも、理由つきの拒否にもならない: %+v", ep, c)
			}
		}
	})

	t.Run("観測用のユーザーは GetBucketObjectLockConfiguration で読む", func(t *testing.T) {
		need(t, "minio")
		minioUser(t, "sakutei-lock-ro", "sakutei-lock-ro-secret-0123", append(append([]string{}, readActions...), "s3:GetBucketObjectLockConfiguration"))
		minioUser(t, "sakutei-lock-nolock", "sakutei-lock-nolock-secret-0123", readActions)
		user := func(ak, sk string) *s3.Session {
			c, err := s3.NewClient(s3.ClientConfig{Endpoint: lab.servers["minio"].url, Credentials: creds(ak, sk)})
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
			t.Errorf("観測用のユーザーの Get: %+v, %v", ol, err)
		}
		if err := ro.PutObjectLock(ctx, "sk-lock-years", s3.ObjectLock{Mode: "COMPLIANCE", Years: 1}); s3.KindOf(err) != s3.KindAuth {
			t.Errorf("観測用のユーザーの Put: %v（want 403）", err)
		}
		if _, err := user("sakutei-lock-nolock", "sakutei-lock-nolock-secret-0123").GetObjectLock(ctx, "sk-lock-years"); s3.KindOf(err) != s3.KindAuth {
			t.Errorf("s3:GetBucketObjectLockConfiguration の無い Get: %v（want 403）", err)
		}
	})
}
