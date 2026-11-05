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

package s3_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/s3"
	"github.com/tjst-t/sakutei/v3/providers/s3/s3test"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const (
	rwKey    = "sakutei-rw"
	rwSecret = "rw-secret-5c1e0a"
	roKey    = "sakutei-ro"
	roSecret = "ro-secret-a83f77"
)

var allFields = []any{"versioning", "policy", "lifecycle", "tags", "object_lock"}

type env struct {
	srv *s3test.Server
	p   *s3.Provider
}

// newEnv は偽の S3 と、それに向けた endpoint fake の provider を作る。観測は読む
// だけのキー、書き込みは書けるキー。extra は endpoint の設定に足す。
func newEnv(t *testing.T, opts s3test.Options, extra map[string]any) *env {
	t.Helper()
	opts.AccessKey, opts.SecretKey = rwKey, rwSecret
	opts.ReadOnlyAccessKey, opts.ReadOnlySecretKey = roKey, roSecret
	srv := s3test.New(opts)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ro"), []byte(roSecret+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "rw"), []byte(rwSecret), 0o600)
	os.WriteFile(filepath.Join(dir, "rw-id"), []byte(rwKey+"\n"), 0o600)
	ep := map[string]any{
		"endpoint": srv.URL, "timeout": "500ms", "fields": allFields,
		"observe": map[string]any{"access_key": roKey, "secret_key_file": filepath.Join(dir, "ro")},
		"write":   map[string]any{"access_key_file": filepath.Join(dir, "rw-id"), "secret_key_file": filepath.Join(dir, "rw")},
	}
	for k, v := range extra {
		if v == nil {
			delete(ep, k)
			continue
		}
		ep[k] = v
	}
	p, err := s3.Module(map[string]any{"endpoints": map[string]any{"fake": ep}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, p: p.(*s3.Provider)}
}

func bucket(name string) core.ID { return core.ID{Type: "s3/bucket", Name: "fake/" + name} }
func wctx() core.WriteContext    { return core.NewWriteContext(nil) }
func rng() core.Range            { return core.Range{Provider: "s3", Args: map[string]string{"endpoint": "fake"}} }
func (e *env) writes() int       { return e.srv.Writes(rwKey) }
func (e *env) writeKeyUse() int {
	c := e.srv.KeyCalls(rwKey)
	return c["READ"] + c["WRITE"]
}

// res は宣言を Schema の正規形にする（load と同じ）。
func (e *env) res(id core.ID, f core.Fields) core.Resource {
	ks := e.p.Schema().Kinds[id.Kind()]
	out := core.Fields{}
	for k, v := range f {
		n, err := core.Normalize(ks.Fields[k].Type, v)
		if err != nil {
			panic(err)
		}
		out[k] = n
	}
	return core.Resource{ID: id, Fields: out}
}

func (e *env) observe(t *testing.T, id core.ID) core.Observation {
	t.Helper()
	o, err := e.p.Observe(context.Background(), []core.ID{id})
	if err != nil {
		t.Fatal(err)
	}
	return o[id]
}

func (e *env) compare(t *testing.T, r core.Resource) plan.Change {
	t.Helper()
	return plan.Compare(r, e.p.Schema().Kinds[r.ID.Kind()], e.observe(t, r.ID))
}

func (e *env) apply(t *testing.T, r core.Resource) error {
	t.Helper()
	return e.p.Apply(context.Background(), wctx(), r.ID, r.Fields, e.observe(t, r.ID))
}

func policyFor(b string) map[string]any {
	return map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Sid": "public-read", "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::" + b + "/*",
	}}}
}

func lifecycle() []any {
	return []any{
		map[string]any{"ID": "expire-logs", "Status": "Enabled", "Filter": map[string]any{"Prefix": "logs/"}, "Expiration": map[string]any{"Days": 30}},
		map[string]any{"ID": "noncurrent", "Status": "Enabled", "Filter": map[string]any{}, "NoncurrentVersionExpiration": map[string]any{"NoncurrentDays": 7}},
	}
}

func appBucket(e *env, name string) core.Resource {
	return e.res(bucket(name), core.Fields{
		"versioning": "Enabled", "policy": policyFor(name), "lifecycle": lifecycle(),
		"tags":        map[string]any{"env": "test", "owner": "sakutei"},
		"object_lock": map[string]any{"enable_on_create": true, "mode": "GOVERNANCE", "days": 1},
	})
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		e := newEnv(t, s3test.Options{}, nil)
		e.srv.CreateBucket("app-old")
		e.srv.SetVersioning("app-old", "Suspended")
		e.srv.CreateBucket("app-full")
		e.srv.PutObject("app-full")
		return contract.Target{
			Provider: e.p,
			Ranges:   []core.Range{rng()},
			Absent:   []core.ID{bucket("nope-bucket")},
			Create: []core.Resource{
				appBucket(e, "app-new"),
				e.res(bucket("app-plain"), core.Fields{"tags": map[string]any{"a": "1"}}),
			},
			Converge: []core.Resource{e.res(bucket("app-old"), core.Fields{"versioning": "Enabled", "policy": map[string]any{}, "lifecycle": []any{}})},
			Observed: []core.ID{bucket("app-old"), bucket("app-full")},
			Faults: map[string]func(){
				"接続を切る":  func() { e.srv.SetUnreachable(true) },
				"5xx":    func() { e.srv.SetFailStatus(http.StatusInternalServerError) },
				"タイムアウト": func() { e.srv.SetDelay(time.Second) },
				"認証の失敗":  func() { e.srv.SetFailStatus(http.StatusForbidden) },
				"壊れた出力":  func() { e.srv.SetBroken(true) },
			},
			Truncate:             func() { e.srv.SetTruncate(true) },
			Writes:               e.writes,
			WriteCredentialReads: e.writeKeyUse,
			Secrets:              []string{rwSecret, roSecret},
			Refuses:              []contract.Refuse{{ID: bucket("app-full")}},
		}
	})
}

// TestCreateAndDrift は作って宣言どおりになり、手動の変更がずれとして見えて戻り、
// 書き方の違い（Principal "*"・ルールの順・years）がずれにならない（E-C1・E-I7）。
func TestCreateAndDrift(t *testing.T) {
	e := newEnv(t, s3test.Options{ObjectLockYearsAsDays: true}, nil)
	r := appBucket(e, "app-a")
	r.Fields["object_lock"] = map[string]any{"enable_on_create": true, "mode": "COMPLIANCE", "years": int64(1)}
	if c := e.compare(t, r); c.Op != plan.Create {
		t.Fatalf("作る前: %s", c.Op)
	}
	if err := e.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("作ったあと: %s %+v", c.Op, c.Fields)
	}
	if mode, days, years, ok := e.srv.ObjectLock("app-a"); !ok || mode != "COMPLIANCE" || days != 365 || years != 0 {
		t.Errorf("Object Lock = %s %d %d %v", mode, days, years, ok)
	}
	e.srv.SetVersioning("app-a", "Suspended")
	e.srv.SetPolicy("app-a", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::app-a/*"}]}`)
	c := e.compare(t, r)
	if c.Op != plan.Update || len(c.Fields) != 2 {
		t.Fatalf("手動の変更のあと: %s %+v", c.Op, c.Fields)
	}
	if err := e.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("戻したあと: %s %+v", c.Op, c.Fields)
	}
	// 書いた形と違う形で持つ endpoint（Principal を {"AWS": "*"} で返す）でもずれない。
	e.srv.SetPolicy("app-a", `{"Statement":[{"Resource":["arn:aws:s3:::app-a/*"],"Action":["s3:GetObject"],"Principal":{"AWS":["*"]},"Effect":"Allow","Sid":"public-read"}],"Version":"2012-10-17"}`)
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("同じ意味の別の書き方: %s %+v", c.Op, c.Fields)
	}
	// 外す：空のポリシー・空のルール・空のタグ。
	clear := e.res(bucket("app-a"), core.Fields{"policy": map[string]any{}, "lifecycle": []any{}, "tags": map[string]any{}, "versioning": "Suspended"})
	if err := e.apply(t, clear); err != nil {
		t.Fatal(err)
	}
	if c := e.compare(t, clear); c.Op != plan.None {
		t.Fatalf("外したあと: %s %+v", c.Op, c.Fields)
	}
	if v, pol, lc, tags, _ := e.srv.Snapshot("app-a"); v != "Suspended" || pol != "" || lc != "" || len(tags) != 0 {
		t.Errorf("外したあとの現実: %q %q %q %v", v, pol, lc, tags)
	}
}

// TestObjectLockApproval は既にあるバケットの保持の設定の変更が、向きによらず承認の
// 要る破壊的変更で、作るとき・宣言どおりのとき・宣言どおりにできないときは何も
// 返さない（E-C1）。
func TestObjectLockApproval(t *testing.T) {
	e := newEnv(t, s3test.Options{}, nil)
	lock := func(m map[string]any) core.Resource { return e.res(bucket("app-l"), core.Fields{"object_lock": m}) }
	gov1 := lock(map[string]any{"enable_on_create": true, "mode": "GOVERNANCE", "days": 1})
	if h := e.p.Classify(gov1.ID, e.observe(t, gov1.ID), gov1.Fields); len(h) != 0 {
		t.Errorf("作るときに %+v", h)
	}
	if err := e.apply(t, gov1); err != nil {
		t.Fatal(err)
	}
	if h := e.p.Classify(gov1.ID, e.observe(t, gov1.ID), gov1.Fields); len(h) != 0 {
		t.Errorf("宣言どおりなのに %+v", h)
	}
	for _, tc := range []struct {
		to   map[string]any
		want string
	}{
		{map[string]any{"mode": "GOVERNANCE", "days": 2}, "1 → 2 日に延ばす"},
		{map[string]any{"mode": "COMPLIANCE", "days": 1}, "GOVERNANCE → COMPLIANCE"},
		{map[string]any{}, "既定の保持（GOVERNANCE 1 日）を外す"},
	} {
		r := lock(tc.to)
		h := e.p.Classify(r.ID, e.observe(t, r.ID), r.Fields)
		if len(h) != 1 || !h[0].NeedsApproval || !h[0].Destructive || !strings.Contains(h[0].Reason, tc.want) {
			t.Errorf("%v: %+v（want 承認の要る破壊的変更 %q）", tc.to, h, tc.want)
		}
	}
	// 外せない Object Lock を外す宣言は承認を求めず、Apply が何も書かずに拒否する。
	off := lock(map[string]any{"enabled": false})
	if h := e.p.Classify(off.ID, e.observe(t, off.ID), off.Fields); len(h) != 0 {
		t.Errorf("外す宣言に %+v", h)
	}
	n := e.writes()
	if err := e.apply(t, off); err == nil || !strings.Contains(err.Error(), "外せない") {
		t.Errorf("外す Apply: %v", err)
	}
	if e.writes() != n {
		t.Error("拒否した Apply が書いた")
	}
	// Object Lock の無い既にあるバケットに enable_on_create は作り直さずに拒否する。
	e.srv.CreateBucket("app-plain")
	plain := e.res(bucket("app-plain"), core.Fields{"object_lock": map[string]any{"enable_on_create": true}})
	if h := e.p.Classify(plain.ID, e.observe(t, plain.ID), plain.Fields); len(h) != 0 {
		t.Errorf("enable_on_create の食い違いに %+v", h)
	}
	if err := e.apply(t, plain); err == nil || !strings.Contains(err.Error(), "作り直さない") {
		t.Errorf("enable_on_create の Apply: %v", err)
	}
	// enable_on_create を外せば承認の要る変更として PutObjectLockConfiguration で掛ける。
	plain = e.res(bucket("app-plain"), core.Fields{"object_lock": map[string]any{}})
	h := e.p.Classify(plain.ID, e.observe(t, plain.ID), plain.Fields)
	if len(h) != 1 || !h[0].NeedsApproval || !strings.Contains(h[0].Reason, "有効にする") {
		t.Errorf("既にあるバケットに掛ける: %+v", h)
	}
	if err := e.apply(t, plain); err != nil {
		t.Fatal(err)
	}
	if c := e.compare(t, plain); c.Op != plan.None {
		t.Errorf("掛けたあと: %s %+v", c.Op, c.Fields)
	}
}

// TestObjectLockWithoutCreateTimeEnable は作るときの有効化を持たない実装でも
// PutObjectLockConfiguration だけで宣言どおりになる。
func TestObjectLockWithoutCreateTimeEnable(t *testing.T) {
	e := newEnv(t, s3test.Options{NoObjectLockOnCreate: true, ObjectLockModes: []string{"COMPLIANCE"}}, nil)
	r := e.res(bucket("app-c"), core.Fields{"object_lock": map[string]any{"mode": "COMPLIANCE", "years": 7}})
	if err := e.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("%s %+v", c.Op, c.Fields)
	}
	if len(e.srv.LockPuts()) != 1 {
		t.Errorf("PutObjectLockConfiguration %d 回", len(e.srv.LockPuts()))
	}
	// endpoint が受けない mode は理由つきで止まる。
	bad := e.res(bucket("app-d"), core.Fields{"object_lock": map[string]any{"mode": "GOVERNANCE", "days": 1}})
	if err := e.apply(t, bad); err == nil || !strings.Contains(err.Error(), "受けない mode") {
		t.Errorf("受けない mode: %v", err)
	}
}

// TestFieldsAndNotImplemented は endpoint の fields に無い設定を読みに行かず、宣言
// すればエラーにし、API を持たない設定は unsupported に出て、宣言すれば Apply が分かる文で
// 止まる（E-C1）。
func TestFieldsAndNotImplemented(t *testing.T) {
	e := newEnv(t, s3test.Options{NotImplemented: []string{s3test.APITagging}, GetStatus: map[string]int{s3test.APILifecycle: 400}},
		map[string]any{"fields": []any{"versioning", "policy", "tags"}})
	e.srv.CreateBucket("app-x")
	o := e.observe(t, bucket("app-x"))
	if o.Availability != core.Present {
		t.Fatalf("%s %s", o.Availability, o.Note)
	}
	if e.srv.Calls("GetBucketLifecycleConfiguration") != 0 || e.srv.Calls("GetObjectLockConfiguration") != 0 {
		t.Error("fields に無い設定を読んだ")
	}
	got := o.Fields["unsupported"].([]any)
	if len(got) != 2 || got[0] != "lifecycle" || got[1] != "tags" {
		t.Errorf("unsupported = %v", got)
	}
	if _, ok := o.Fields["tags"]; ok {
		t.Error("NotImplemented の tags が観測にある")
	}
	diags := e.p.Validate([]core.Resource{
		e.res(bucket("app-x"), core.Fields{"lifecycle": []any{}}),
		e.res(bucket("app-y"), core.Fields{"object_lock": map[string]any{}}),
		e.res(bucket("app-z"), core.Fields{"creation_date": "x"}),
	}, []core.Range{rng()})
	if len(diags) != 3 || !strings.Contains(diags[0].Message, "lifecycle を扱わない") || !strings.Contains(diags[2].Message, "観測だけ") {
		t.Errorf("診断: %v", diags)
	}
	r := e.res(bucket("app-x"), core.Fields{"tags": map[string]any{"a": "b"}})
	if err := e.apply(t, r); err == nil || !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("API を持たない設定の Apply: %v", err)
	}
	// 既定の fields は object_lock を扱わない（書いたときだけ）。
	d := newEnv(t, s3test.Options{}, map[string]any{"fields": nil})
	d.srv.CreateBucket("app-x")
	d.observe(t, bucket("app-x"))
	if d.srv.Calls("GetObjectLockConfiguration") != 0 {
		t.Error("既定の fields で Object Lock を読んだ")
	}
	diags = d.p.Validate([]core.Resource{d.res(bucket("app-x"), core.Fields{"object_lock": map[string]any{}})}, []core.Range{rng()})
	if len(diags) != 1 || !strings.Contains(diags[0].Message, "fields に書いたときだけ") {
		t.Errorf("既定の fields の object_lock: %v", diags)
	}
}

// TestStorageClassAliases は endpoint 独自の StorageClass の名前を読み替えて比べる。
func TestStorageClassAliases(t *testing.T) {
	e := newEnv(t, s3test.Options{StorageClassRewrite: map[string]string{"GLACIER": "VENDOR_COLD"}},
		map[string]any{"storage_class_aliases": map[string]any{"VENDOR_COLD": "GLACIER"}})
	r := e.res(bucket("app-t"), core.Fields{"lifecycle": []any{map[string]any{"ID": "cold", "Status": "Enabled", "Filter": map[string]any{},
		"Transition": map[string]any{"Days": 30, "StorageClass": "GLACIER"}}}})
	if err := e.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("%s %+v", c.Op, c.Fields)
	}
	if _, _, lc, _, _ := e.srv.Snapshot("app-t"); !strings.Contains(lc, "VENDOR_COLD") {
		t.Errorf("endpoint の持つ形: %s", lc)
	}
	// 読み替えなければ読み戻しで分かる文で止まる。
	n := newEnv(t, s3test.Options{StorageClassRewrite: map[string]string{"GLACIER": "VENDOR_COLD"}}, nil)
	if err := n.apply(t, n.res(r.ID, r.Fields)); err == nil || !strings.Contains(err.Error(), "storage_class_aliases") {
		t.Errorf("読み替え無し: %v", err)
	}
}

// TestDelete は空のバケットを消し、中身のあるバケットは何も書かずに拒否する（E-F1）。
func TestDelete(t *testing.T) {
	e := newEnv(t, s3test.Options{}, nil)
	e.srv.CreateBucket("app-empty")
	e.srv.CreateBucket("app-full")
	e.srv.PutObject("app-full")
	if err := e.p.Delete(context.Background(), wctx(), bucket("app-empty"), core.Observation{}); err != nil || e.srv.Has("app-empty") {
		t.Fatalf("空のバケット: %v %v", err, e.srv.Has("app-empty"))
	}
	if err := e.p.Delete(context.Background(), wctx(), bucket("app-empty"), core.Observation{}); err != nil {
		t.Errorf("無いバケットの Delete: %v", err)
	}
	n := e.writes()
	err := e.p.Delete(context.Background(), wctx(), bucket("app-full"), core.Observation{})
	var re *core.RefuseError
	if !errors.As(err, &re) || !e.srv.Has("app-full") || e.writes() != n {
		t.Errorf("中身のあるバケット: %v（書いた %d 回）", err, e.writes()-n)
	}
	if !e.p.Schema().Kinds["bucket"].DestructiveDelete {
		t.Error("バケットは中身を失う type")
	}
}

// TestValidateRanges は Range の args・重なり・Range の外の宣言をエラーにする（E-I6）。
func TestValidateRanges(t *testing.T) {
	e := newEnv(t, s3test.Options{}, nil)
	rg := func(args map[string]string) core.Range { return core.Range{Provider: "s3", Args: args} }
	for _, tc := range []struct {
		ranges []core.Range
		res    []core.Resource
		want   string
	}{
		{[]core.Range{rg(map[string]string{"endpoint": "fake", "prefix": "app-"})}, []core.Resource{{ID: bucket("other")}}, "どの Range"},
		{[]core.Range{rg(map[string]string{"endpoint": "fake", "prefix": "app-"}), rg(map[string]string{"endpoint": "fake", "prefix": "app-x"})}, nil, "重なる"},
		{[]core.Range{rg(map[string]string{"endpoint": "fake"}), rg(map[string]string{"endpoint": "fake", "prefix": "b"})}, nil, "重なる"},
		{[]core.Range{rg(map[string]string{"endpoint": "nope"})}, nil, "settings.endpoints に無い"},
		{[]core.Range{rg(map[string]string{"prefix": "a"})}, nil, "args.endpoint"},
		{[]core.Range{rg(map[string]string{"endpoint": "fake", "bucket": "a"})}, nil, "endpoint と prefix だけ"},
		{[]core.Range{rg(map[string]string{"endpoint": "fake", "prefix": "A_"})}, nil, "使えない文字"},
		{[]core.Range{rng()}, []core.Resource{{ID: core.ID{Type: "s3/bucket", Name: "fake/Bad_Name"}}}, "バケット名でない"},
		{[]core.Range{rng()}, []core.Resource{{ID: core.ID{Type: "s3/object", Name: "fake/a-b"}}}, "type"},
		{[]core.Range{rng()}, []core.Resource{e.res(bucket("app-v"), core.Fields{"versioning": "On"})}, "versioning"},
		{[]core.Range{rng()}, []core.Resource{e.res(bucket("app-v"), core.Fields{"tags": map[string]any{"a": 1}})}, "文字列"},
		{[]core.Range{rng()}, []core.Resource{e.res(bucket("app-v"), core.Fields{"lifecycle": []any{map[string]any{"ID": "a"}}})}, "Status"},
		{[]core.Range{rng()}, []core.Resource{e.res(bucket("app-v"), core.Fields{"policy": map[string]any{"Statement": nil}})}, ""},
	} {
		diags := e.p.Validate(tc.res, tc.ranges)
		if tc.want == "" {
			continue
		}
		found := false
		for _, d := range diags {
			found = found || (d.Severity == core.Reject && strings.Contains(d.Message, tc.want))
		}
		if !found {
			t.Errorf("%v %v: %v（want %q）", tc.ranges, tc.res, diags, tc.want)
		}
	}
	// 2 つの接頭辞に分けた Range（重ならない）の List は自分の接頭辞だけを返す。
	e.srv.CreateBucket("app-one")
	e.srv.CreateBucket("db-one")
	two := []core.Range{rg(map[string]string{"endpoint": "fake", "prefix": "app-"}), rg(map[string]string{"endpoint": "fake", "prefix": "db-"})}
	if d := e.p.Validate(nil, two); len(d) != 0 {
		t.Fatalf("%v", d)
	}
	l, err := e.p.List(context.Background(), two[1])
	if err != nil || !l.Complete || len(l.IDs) != 1 || l.IDs[0] != bucket("db-one") {
		t.Errorf("List: %+v %v", l, err)
	}
}

// TestModuleSettings は settings の不正な値をエラーにする（secret key そのものは書けない）。
func TestModuleSettings(t *testing.T) {
	ok := map[string]any{"endpoint": "http://127.0.0.1:9000", "observe": map[string]any{"access_key": "a", "secret_key_file": "/x"}}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range ok {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for _, tc := range []struct {
		settings map[string]any
		want     string
	}{
		{map[string]any{}, "settings.endpoints"},
		{map[string]any{"endpoints": map[string]any{"a/b": ok}}, "名前"},
		{map[string]any{"endpoints": map[string]any{"a": with("secret_key", "x")}}, "知らない field"},
		{map[string]any{"endpoints": map[string]any{"a": with("observe", map[string]any{"access_key": "a", "secret_key": "x"})}}, "secret key そのものは書けない"},
		{map[string]any{"endpoints": map[string]any{"a": with("observe", map[string]any{"secret_key_file": "/x"})}}, "access_key"},
		{map[string]any{"endpoints": map[string]any{"a": with("endpoint", "s3.example.net")}}, "http://host:port"},
		{map[string]any{"endpoints": map[string]any{"a": with("api_base_path", "s3")}}, "api_base_path"},
		{map[string]any{"endpoints": map[string]any{"a": with("fields", []any{"acl"})}}, "fields[0]"},
		{map[string]any{"endpoints": map[string]any{"a": with("addressing", "dns")}}, "addressing"},
		{map[string]any{"endpoints": map[string]any{"a": with("insecure_skip_verify", true), "b": ok}, "x": 1}, "知らない field"},
	} {
		if _, err := s3.Module(tc.settings); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v（want %q）", tc.settings, err, tc.want)
		}
	}
	if _, err := s3.Module(map[string]any{"endpoints": map[string]any{"a": ok}}); err != nil {
		t.Errorf("正しい settings: %v", err)
	}
}

// TestAPIBasePath は API のパスの前置きの下の endpoint に対して動かす。
func TestAPIBasePath(t *testing.T) {
	e := newEnv(t, s3test.Options{BasePath: "/gw/s3"}, map[string]any{"api_base_path": "/gw/s3/"})
	r := e.res(bucket("app-b"), core.Fields{"versioning": "Enabled"})
	if err := e.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("%s %+v", c.Op, c.Fields)
	}
	// 前置きを書かなければ届かない（Unknown。Absent と読まない）。
	n := newEnv(t, s3test.Options{BasePath: "/gw/s3"}, nil)
	if o := n.observe(t, bucket("app-b")); o.Availability != core.Unknown {
		t.Errorf("前置き無し: %s", o.Availability)
	}
}

// TestPagination は ListBuckets を最後のページまで辿る。
func TestPagination(t *testing.T) {
	e := newEnv(t, s3test.Options{PageSize: 2}, nil)
	for _, b := range []string{"app-1", "app-2", "app-3", "app-4", "app-5"} {
		e.srv.CreateBucket(b)
	}
	l, err := e.p.List(context.Background(), rng())
	if err != nil || !l.Complete || len(l.IDs) != 5 {
		t.Fatalf("%+v %v", l, err)
	}
	if o := e.observe(t, bucket("app-5")); o.Availability != core.Present {
		t.Errorf("最後のページのバケット: %s %s", o.Availability, o.Note)
	}
}
