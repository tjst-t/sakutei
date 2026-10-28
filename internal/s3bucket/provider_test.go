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

package s3bucket_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket/s3test"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

const (
	samplePolicy    = `{"Statement":[{"Action":["s3:GetObject"],"Effect":"Allow","Principal":{"AWS":["*"]},"Resource":["arn:aws:s3:::sakutei-contract/*"]}],"Version":"2012-10-17"}`
	sampleLifecycle = `[{"Expiration":{"Days":"30"},"Filter":{"Prefix":"logs/"},"ID":"expire-logs","Status":"Enabled"}]`
	sampleTags      = `{"env":"test","team":"infra"}`
)

func staticCreds(ak, sk string) func(context.Context) (s3bucket.Credentials, error) {
	return func(context.Context) (s3bucket.Credentials, error) {
		return s3bucket.Credentials{AccessKey: ak, SecretKey: sk}, nil
	}
}

// newProvider は偽サーバと、それに向けた "fake" endpoint の Provider を返す。
func newProvider(t *testing.T, opts s3test.Options, prefix string) (*s3test.Server, *s3bucket.Provider) {
	t.Helper()
	srv := s3test.New(opts)
	t.Cleanup(srv.Close)
	c, err := s3bucket.NewClient(s3bucket.ClientConfig{
		Endpoint: srv.URL, Credentials: staticCreds("test-access", "test-secret"), Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, s3bucket.New(map[string]s3bucket.Endpoint{"fake": {Client: c, BucketPrefix: prefix}}, s3bucket.Options{})
}

func plan(t *testing.T, p *s3bucket.Provider, id string, fields map[string]string) (core.Plan, error) {
	t.Helper()
	obs, err := p.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("Read %s: %v", id, err)
	}
	return diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: core.NewID(s3bucket.Name, core.NewScope(nil), id), Fields: fields},
		cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
}

func converge(t *testing.T, p *s3bucket.Provider, id string, fields map[string]string) {
	t.Helper()
	pl, err := plan(t, p, id, fields)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := p.Apply(context.Background(), pl); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	pl, err = plan(t, p, id, fields)
	if err != nil || len(pl.Ops) != 0 {
		t.Fatalf("after Apply: plan = %+v, %v; want converged", pl.Ops, err)
	}
}

// TestAPIBasePath は API のパスの前置きを変えた偽サーバ（模擬）で、設定した
// 前置きで作る・観測する・一覧する・消すが動き、前置きを省いた（既定の）
// Client では観測できないこと（#160）。
func TestAPIBasePath(t *testing.T) {
	srv := s3test.New(s3test.Options{BasePath: "/gw/s3"})
	t.Cleanup(srv.Close)
	newP := func(basePath string) *s3bucket.Provider {
		c, err := s3bucket.NewClient(s3bucket.ClientConfig{
			Endpoint: srv.URL, APIBasePath: basePath, Credentials: staticCreds("test-access", "test-secret"), Timeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s3bucket.New(map[string]s3bucket.Endpoint{"fake": {Client: c}}, s3bucket.Options{})
	}
	for _, bp := range []string{"/gw/s3", "/gw/s3/"} {
		p := newP(bp)
		converge(t, p, "fake/based", map[string]string{s3bucket.FieldVersioning: "Enabled", s3bucket.FieldTags: sampleTags})
		if !srv.Has("based") {
			t.Fatalf("base %q: the bucket was not created", bp)
		}
		lr, err := p.List(context.Background(), core.NewScope(nil))
		if err != nil || lr.Status != core.ListComplete || len(lr.IDs) != 1 || lr.IDs[0].Local != "fake/based" {
			t.Errorf("base %q: List = %+v, %v", bp, lr, err)
		}
	}
	obs, err := newP("").Read(context.Background(), "fake/based")
	if err == nil && obs.Availability == core.AvailabilityOk {
		t.Errorf("Read without the base path = %+v; want not observable", obs)
	}
}

func TestValidateProvider(t *testing.T) {
	_, p := newProvider(t, s3test.Options{}, "")
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	caps := p.Capabilities()
	if !caps.TrueObserve || !caps.List || !caps.BulkObserve || !caps.Pruneable {
		t.Errorf("Capabilities = %+v, want the full tier, pruneable", caps)
	}
}

func TestContract(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{}, "")
	srv.CreateBucket("other-bucket")
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		SampleID: "fake/sakutei-contract",
		SampleDesired: map[string]string{
			s3bucket.FieldEndpoint:   "fake",
			s3bucket.FieldBucket:     "sakutei-contract",
			s3bucket.FieldVersioning: "Enabled",
			s3bucket.FieldPolicy:     samplePolicy,
			s3bucket.FieldLifecycle:  sampleLifecycle,
			s3bucket.FieldTags:       sampleTags,
		},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
}

func TestBulkEquivalence(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{NotImplemented: []string{s3test.APITagging}}, "")
	converge(t, p, "fake/bulk-a", map[string]string{s3bucket.FieldVersioning: "Enabled", s3bucket.FieldPolicy: samplePolicy})
	srv.CreateBucket("bulk-b")
	srv.SetLifecycleXML("bulk-b", `<LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`)
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)

	before := srv.Calls("ListBuckets")
	if _, err := p.BulkObserve(context.Background(), core.NewScope(nil)); err != nil {
		t.Fatal(err)
	}
	if n := srv.Calls("ListBuckets") - before; n != 1 {
		t.Errorf("BulkObserve called ListBuckets %d times, want 1", n)
	}
}

func TestRead_ObservedOnlyAndAbsent(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{}, "")
	obs, err := p.Read(context.Background(), "fake/nope")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State != nil {
		t.Fatalf("Read of a missing bucket = %+v, %v; want Ok and absent", obs, err)
	}
	srv.CreateBucket("plain")
	obs, err = p.Read(context.Background(), "fake/plain")
	if err != nil || obs.State == nil {
		t.Fatalf("Read = %+v, %v", obs, err)
	}
	want := map[string]string{
		s3bucket.FieldEndpoint: "fake", s3bucket.FieldBucket: "plain", s3bucket.FieldVersioning: "",
		s3bucket.FieldPolicy: "", s3bucket.FieldLifecycle: "", s3bucket.FieldTags: "", s3bucket.FieldUnsupported: "",
	}
	for k, v := range want {
		if got := fmt.Sprint(obs.State[k].Value); got != v {
			t.Errorf("State[%s] = %q, want %q", k, got, v)
		}
	}
	if fmt.Sprint(obs.State[s3bucket.FieldCreationDate].Value) == "" {
		t.Error("creation_date is empty")
	}
	if s := p.Schema().Fields[s3bucket.FieldCreationDate]; !s.Computed {
		t.Error("creation_date must be Computed")
	}
}

func TestPlan_NormalisedFieldsDoNotDrift(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{}, "")
	id := "fake/norm"
	desired := map[string]string{
		s3bucket.FieldPolicy: `{
		  "Version": "2012-10-17",
		  "Statement": {"Effect": "Allow", "Principal": {"AWS": "*"}, "Action": "s3:GetObject", "Resource": "arn:aws:s3:::norm/*"}
		}`,
		s3bucket.FieldLifecycle: `[{"ID": "a", "Status": "Enabled", "Expiration": {"Days": 7}}, {"ID": "b", "Status": "Disabled", "Filter": {"Prefix": "tmp/"}, "Expiration": {"Days": 1}}]`,
		s3bucket.FieldTags:      `{"b": "2", "a": "1"}`,
	}
	converge(t, p, id, desired)

	// 同じ意味の別の書き方（キー順・空白・1 要素の配列・ルールの順・数と文字列）は drift にしない。
	srv.SetPolicy("norm", `{"Statement":[{"Resource":["arn:aws:s3:::norm/*"],"Action":["s3:GetObject"],"Principal":{"AWS":["*"]},"Effect":"Allow"}],"Version":"2012-10-17"}`)
	srv.SetLifecycleXML("norm", `<LifecycleConfiguration><Rule><ID>b</ID><Filter><Prefix>tmp/</Prefix></Filter><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule><Rule><ID>a</ID><Filter></Filter><Status>Enabled</Status><Expiration><Days>7</Days></Expiration></Rule></LifecycleConfiguration>`)
	for i := 0; i < 3; i++ {
		if pl, err := plan(t, p, id, desired); err != nil || len(pl.Ops) != 0 {
			t.Fatalf("cycle %d: plan = %+v, %v; want no drift", i, pl.Ops, err)
		}
	}

	// 中身を手で変えたら drift で、Apply で戻る。
	srv.SetPolicy("norm", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::norm/*"}]}`)
	srv.SetVersioning("norm", "Enabled")
	srv.SetTags("norm", map[string]string{"a": "changed"})
	pl, err := plan(t, p, id, desired)
	if err != nil || len(pl.Ops) != 1 {
		t.Fatalf("plan = %+v, %v; want one Update", pl.Ops, err)
	}
	var fields []string
	for _, f := range pl.Ops[0].Fields {
		fields = append(fields, f.Field)
	}
	if got := strings.Join(fields, ","); got != "policy,tags" {
		t.Errorf("drifted fields = %s, want policy,tags (versioning is not declared, so it is not managed)", got)
	}
	converge(t, p, id, desired)
	if v, _, _, _, _ := srv.Snapshot("norm"); v != "Enabled" {
		t.Errorf("versioning = %q: an undeclared field was touched", v)
	}
}

func TestPlan_EmptyMeansRemove(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{}, "")
	converge(t, p, "fake/rm-bucket", map[string]string{s3bucket.FieldPolicy: samplePolicy, s3bucket.FieldLifecycle: sampleLifecycle, s3bucket.FieldTags: sampleTags})
	converge(t, p, "fake/rm-bucket", map[string]string{s3bucket.FieldPolicy: "", s3bucket.FieldLifecycle: "[]", s3bucket.FieldTags: "{}"})
	if _, pol, lc, tags, _ := srv.Snapshot("rm-bucket"); pol != "" || lc != "" || len(tags) != 0 {
		t.Errorf("after declaring empty: policy=%q lifecycle=%q tags=%v; want all removed", pol, lc, tags)
	}
}

func TestNotImplemented_DeclaredIsUnobservable_UndeclaredIgnored(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{NotImplemented: []string{s3test.APILifecycle, s3test.APITagging}}, "")
	srv.CreateBucket("ni-bucket")
	obs, err := p.Read(context.Background(), "fake/ni-bucket")
	if err != nil || obs.State == nil {
		t.Fatalf("Read = %+v, %v", obs, err)
	}
	if got := fmt.Sprint(obs.State[s3bucket.FieldUnsupported].Value); got != "lifecycle,tags" {
		t.Errorf("unsupported = %q, want lifecycle,tags", got)
	}
	if _, ok := obs.State[s3bucket.FieldLifecycle]; ok {
		t.Error("State has lifecycle although the endpoint does not implement it")
	}
	// 宣言していなければ無視（versioning だけで収束する）。
	converge(t, p, "fake/ni-bucket", map[string]string{s3bucket.FieldVersioning: "Enabled"})
	// 宣言していれば観測できない（分かる文で）。
	_, err = plan(t, p, "fake/ni-bucket", map[string]string{s3bucket.FieldVersioning: "Enabled", s3bucket.FieldLifecycle: sampleLifecycle})
	if err == nil || !strings.Contains(err.Error(), "cannot observe lifecycle") || !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("plan with a declared unsupported field: err = %v; want cannot observe lifecycle … NotImplemented", err)
	}
	// 作るときに設定しようとしても分かる文の PermanentError。
	pl, _ := plan(t, p, "fake/ni-new", map[string]string{s3bucket.FieldTags: sampleTags})
	err = p.Apply(context.Background(), pl)
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("Apply of tags on an endpoint without tagging: %v; want a PermanentError that says so", err)
	}
}

func TestDelete_NotEmptyIsKept(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{}, "")
	srv.CreateBucket("full")
	srv.PutObject("full")
	del := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Destructive: true, Target: core.NewID(s3bucket.Name, core.NewScope(nil), "fake/full")}}}
	err := p.Apply(context.Background(), del)
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "BucketNotEmpty") {
		t.Fatalf("Delete of a non-empty bucket: %v; want a PermanentError with BucketNotEmpty", err)
	}
	if !srv.Has("full") {
		t.Fatal("the non-empty bucket was deleted")
	}

	srv.CreateBucket("empty")
	del.Ops[0].Target = core.NewID(s3bucket.Name, core.NewScope(nil), "fake/empty")
	for i := 0; i < 2; i++ { // 2 回目は無いので成功（冪等）。
		if err := p.Apply(context.Background(), del); err != nil {
			t.Fatalf("Delete #%d: %v", i+1, err)
		}
	}
	if srv.Has("empty") {
		t.Error("the empty bucket is still there")
	}
}

func TestUnobservable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*s3test.Server)
		creds     func(context.Context) (s3bucket.Credentials, error)
		permanent bool
	}{
		{name: "unreachable", setup: func(s *s3test.Server) { s.SetUnreachable(true) }},
		{name: "5xx", setup: func(s *s3test.Server) { s.SetFailStatus(http.StatusServiceUnavailable) }},
		{name: "bad secret", creds: staticCreds("test-access", "wrong-secret-value"), permanent: true},
		{name: "unknown access key", creds: staticCreds("nobody", "test-secret"), permanent: true},
		{name: "no credentials", creds: func(context.Context) (s3bucket.Credentials, error) {
			return s3bucket.Credentials{}, errors.New("secret not found: s3/sk")
		}, permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := s3test.New(s3test.Options{})
			defer srv.Close()
			srv.CreateBucket("bucket-1")
			creds := tc.creds
			if creds == nil {
				creds = staticCreds("test-access", "test-secret")
			}
			c, err := s3bucket.NewClient(s3bucket.ClientConfig{Endpoint: srv.URL, Credentials: creds, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			p := s3bucket.New(map[string]s3bucket.Endpoint{"fake": {Client: c}}, s3bucket.Options{})
			if tc.setup != nil {
				tc.setup(srv)
			}
			obs, err := p.Read(context.Background(), "fake/bucket-1")
			if err == nil || obs.Availability != core.AvailabilityUnknown {
				t.Fatalf("Read = %+v, %v; want Unknown with an error", obs, err)
			}
			var pe *provider.PermanentError
			if errors.As(err, &pe) != tc.permanent {
				t.Errorf("Read error %v: permanent = %v, want %v", err, !tc.permanent, tc.permanent)
			}
			if strings.Contains(err.Error(), "wrong-secret-value") || strings.Contains(err.Error(), "test-secret") {
				t.Errorf("the error shows a secret key: %v", err)
			}
			if _, err := p.List(context.Background(), core.NewScope(nil)); err == nil {
				t.Error("List succeeded while the endpoint cannot be observed")
			}
			if _, err := p.BulkObserve(context.Background(), core.NewScope(nil)); err == nil {
				t.Error("BulkObserve succeeded while the endpoint cannot be observed")
			}
		})
	}
}

func TestList_PaginatesAndHonoursPrefix(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{PageSize: 2}, "app-")
	for _, b := range []string{"app-a", "app-b", "app-c", "other-x", "app-d", "zzz"} {
		srv.CreateBucket(b)
	}
	res, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil || res.Status != core.ListComplete {
		t.Fatalf("List = %+v, %v", res, err)
	}
	var got []string
	for _, id := range res.IDs {
		got = append(got, id.Local)
	}
	if s := strings.Join(got, ","); s != "fake/app-a,fake/app-b,fake/app-c,fake/app-d" {
		t.Errorf("List = %s", s)
	}
	if err := p.ValidateIntent("fake/other-x", nil); err == nil {
		t.Error("ValidateIntent accepted a bucket outside bucket_prefix")
	}
}

func TestList_Scope(t *testing.T) {
	srv := s3test.New(s3test.Options{})
	defer srv.Close()
	srv.CreateBucket("scoped")
	c, _ := s3bucket.NewClient(s3bucket.ClientConfig{Endpoint: srv.URL, Credentials: staticCreds("test-access", "test-secret")})
	tokyo := core.NewScope(map[string]string{"site": "tokyo"})
	p := s3bucket.New(map[string]s3bucket.Endpoint{"fake": {Client: c}}, s3bucket.Options{Scope: tokyo})
	res, err := p.List(context.Background(), tokyo)
	if err != nil || len(res.IDs) != 1 || !res.IDs[0].Scope.Equal(tokyo) {
		t.Fatalf("List = %+v, %v; want the id in scope site=tokyo", res, err)
	}
}

func TestApply_ReadBackMismatchIsPermanent(t *testing.T) {
	// endpoint がポリシーに Statement を足して持つ：宣言の形では永久に一致しない。
	_, p := newProvider(t, s3test.Options{PolicyRewrite: func(s string) string {
		return strings.Replace(s, `"Statement":[`, `"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:DeleteBucket","Resource":"arn:aws:s3:::rw-bucket"},`, 1)
	}}, "")
	pl, _ := plan(t, p, "fake/rw-bucket", map[string]string{s3bucket.FieldPolicy: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::rw-bucket/*"}]}`})
	err := p.Apply(context.Background(), pl)
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "keeps it in another form") || !strings.Contains(err.Error(), "s3:DeleteBucket") {
		t.Errorf("Apply = %v; want a PermanentError showing the endpoint's form", err)
	}
}

func TestValidateIntent(t *testing.T) {
	_, p := newProvider(t, s3test.Options{}, "")
	for _, tc := range []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"nobucket", nil, "want <endpoint>/<bucket>"},
		{"other/b-1", nil, `no endpoint "other"`},
		{"fake/Bad_Name", nil, "not a valid bucket name"},
		{"fake/192.168.0.1", nil, "not a valid bucket name"},
		{"fake/ok-1", map[string]string{"size": "1"}, `unknown field "size"`},
		{"fake/ok-1", map[string]string{"creation_date": "x"}, "observed only"},
		{"fake/ok-1", map[string]string{"bucket": "ok-2"}, "must equal the bucket"},
		{"fake/ok-1", map[string]string{"versioning": "On"}, "want Enabled or Suspended"},
		{"fake/ok-1", map[string]string{"policy": "{"}, "not valid JSON"},
		{"fake/ok-1", map[string]string{"policy": "[]"}, "JSON object"},
		{"fake/ok-1", map[string]string{"lifecycle": `[{"Status":"Enabled"}]`}, "ID is required"},
		{"fake/ok-1", map[string]string{"lifecycle": `[{"ID":"a","Status":"On"}]`}, "Status must be"},
		{"fake/ok-1", map[string]string{"lifecycle": `{"ID":"a"}`}, "JSON array"},
		{"fake/ok-1", map[string]string{"tags": `{"a":1}`}, "must be a string"},
		{"fake/ok-1", map[string]string{"endpoint": "fake", "bucket": "ok-1", "versioning": "Suspended", "tags": `{"a":"b"}`}, ""},
	} {
		err := p.ValidateIntent(tc.id, tc.fields)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("ValidateIntent(%s, %v) = %v; want nil", tc.id, tc.fields, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("ValidateIntent(%s, %v) = %v; want %q", tc.id, tc.fields, err, tc.want)
		}
	}
}

func TestBulkObserve_OneEndpointDownKeepsTheOthers(t *testing.T) {
	up := s3test.New(s3test.Options{})
	defer up.Close()
	down := s3test.New(s3test.Options{})
	defer down.Close()
	up.CreateBucket("bucket-up")
	down.CreateBucket("bucket-down")
	eps := map[string]s3bucket.Endpoint{}
	for name, srv := range map[string]*s3test.Server{"up": up, "down": down} {
		c, err := s3bucket.NewClient(s3bucket.ClientConfig{Endpoint: srv.URL, Credentials: staticCreds("test-access", "test-secret"), Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		eps[name] = s3bucket.Endpoint{Client: c}
	}
	p := s3bucket.New(eps, s3bucket.Options{})
	down.SetUnreachable(true)

	bo, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("BulkObserve with one endpoint down: %v", err)
	}
	if o, ok := bo.Observations[core.NewID(s3bucket.Name, core.NewScope(nil), "up/bucket-up").Key()]; !ok || o.Availability != core.AvailabilityOk {
		t.Errorf("up/bucket-up = %+v, %v; want observed Ok", o, ok)
	}
	if _, ok := bo.Observations[core.NewID(s3bucket.Name, core.NewScope(nil), "down/bucket-down").Key()]; ok {
		t.Error("down/bucket-down is in the bulk result although its endpoint is down")
	}
	// SDK の経路: 結果に無いものは Read に回り、Unknown（absent ではない）。
	co := sdk.NewCycleObserver(p, core.NewScope(nil))
	obs, err := co.Observe(context.Background(), core.NewID(s3bucket.Name, core.NewScope(nil), "down/bucket-down"))
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Observe(down/bucket-down) = %+v, %v; want Unknown with an error", obs, err)
	}
	// List は complete-or-error のまま（prune を止める）。
	if _, err := p.List(context.Background(), core.NewScope(nil)); err == nil {
		t.Error("List succeeded with one endpoint down")
	}
	up.SetUnreachable(true)
	if _, err := p.BulkObserve(context.Background(), core.NewScope(nil)); err == nil {
		t.Error("BulkObserve succeeded with every endpoint down")
	}
}

// newProviderEP は偽サーバと、ep（Client を除く）の設定の "fake" endpoint の
// Provider を返す。
func newProviderEP(t *testing.T, opts s3test.Options, ep s3bucket.Endpoint) (*s3test.Server, *s3bucket.Provider) {
	t.Helper()
	srv := s3test.New(opts)
	t.Cleanup(srv.Close)
	c, err := s3bucket.NewClient(s3bucket.ClientConfig{
		Endpoint: srv.URL, Credentials: staticCreds("test-access", "test-secret"), Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ep.Client = c
	return srv, s3bucket.New(map[string]s3bucket.Endpoint{"fake": ep}, s3bucket.Options{})
}

// endpoint が versioning の GET を 400 で断る：既定（4 つ全部）ではバケット全体が
// 観測できず、fields で外せば Get を呼ばずに観測が続く。
func TestFields_ExcludedSettingIsNotRead(t *testing.T) {
	opts := s3test.Options{GetStatus: map[string]int{s3test.APIVersioning: http.StatusBadRequest}}

	srv, p := newProviderEP(t, opts, s3bucket.Endpoint{})
	srv.CreateBucket("f-bucket")
	if obs, err := p.Read(context.Background(), "fake/f-bucket"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("without fields: Read = %+v, %v; want unobservable", obs, err)
	}

	fields := []string{s3bucket.FieldPolicy, s3bucket.FieldLifecycle, s3bucket.FieldTags}
	srv, p = newProviderEP(t, opts, s3bucket.Endpoint{Fields: fields})
	srv.CreateBucket("f-bucket")
	obs, err := p.Read(context.Background(), "fake/f-bucket")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State == nil {
		t.Fatalf("with fields: Read = %+v, %v; want observed", obs, err)
	}
	if n := srv.Calls("GetBucketVersioning"); n != 0 {
		t.Errorf("GetBucketVersioning called %d times; want 0 (not in fields)", n)
	}
	if got := fmt.Sprint(obs.State[s3bucket.FieldUnsupported].Value); got != "versioning" {
		t.Errorf("unsupported = %q, want versioning", got)
	}
	if _, ok := obs.State[s3bucket.FieldVersioning]; ok {
		t.Error("State has versioning although it is not in fields")
	}
	// BulkObserve も同じ（Read と等価）。
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)
	if n := srv.Calls("GetBucketVersioning"); n != 0 {
		t.Errorf("GetBucketVersioning called %d times after BulkObserve; want 0", n)
	}
	// 宣言しなければ収束する。
	converge(t, p, "fake/f-bucket", map[string]string{s3bucket.FieldTags: sampleTags})
	// 宣言すれば計画の時点でエラー（読み込みの検査は作る前のバケットでも）。
	declared := map[string]string{s3bucket.FieldVersioning: "Enabled"}
	_, err = plan(t, p, "fake/f-bucket", declared)
	if err == nil || !strings.Contains(err.Error(), "cannot manage versioning") || !strings.Contains(err.Error(), "fields") {
		t.Errorf("plan with versioning declared: err = %v; want cannot manage versioning … fields", err)
	}
	for _, id := range []string{"fake/f-bucket", "fake/f-new"} {
		if err := p.ValidateIntent(id, declared); err == nil || !strings.Contains(err.Error(), "cannot manage versioning") {
			t.Errorf("ValidateIntent %s with versioning declared: err = %v; want cannot manage versioning", id, err)
		}
	}
	if n := srv.Calls("GetBucketVersioning"); n != 0 {
		t.Errorf("GetBucketVersioning called %d times; want 0", n)
	}
	// Apply を直に呼ばれても設定しない。
	err = p.Apply(context.Background(), core.Plan{Ops: []core.Op{{
		Kind: core.OpKindUpdate, Target: core.NewID(s3bucket.Name, core.NewScope(nil), "fake/f-bucket"),
		Fields: []core.FieldDiff{{Field: s3bucket.FieldVersioning, To: "Enabled"}},
	}}})
	var pe *provider.PermanentError
	if !errors.As(err, &pe) {
		t.Errorf("Apply of versioning not in fields = %v; want a PermanentError", err)
	}
	if v, _, _, _, _ := srv.Snapshot("f-bucket"); v != "" {
		t.Errorf("versioning = %q; want untouched", v)
	}
}

// endpoint が lifecycle の StorageClass を独自の名前に書き換えて持ち、PUT では
// 標準の名前しか受け付けない：alias が無ければ Apply の読み戻しで止まり、alias で
// 読み替えれば drift にならない。
func TestStorageClassAliases_NoPerpetualDrift(t *testing.T) {
	opts := s3test.Options{StorageClassRewrite: map[string]string{"GLACIER": "COLD_TIER", "STANDARD_IA": "WARM_TIER"}}
	lc := `[{"ID":"archive","Status":"Enabled","Filter":{"Prefix":"logs/"},"Transition":[{"Days":30,"StorageClass":"STANDARD_IA"},{"Days":90,"StorageClass":"GLACIER"}],"NoncurrentVersionTransition":{"NoncurrentDays":7,"StorageClass":"GLACIER"}}]`
	want := map[string]string{s3bucket.FieldLifecycle: lc}

	_, p := newProviderEP(t, opts, s3bucket.Endpoint{})
	pl, _ := plan(t, p, "fake/sc-bucket", want)
	err := p.Apply(context.Background(), pl)
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "storage_class_aliases") || !strings.Contains(err.Error(), "COLD_TIER") {
		t.Errorf("without aliases: Apply = %v; want a PermanentError pointing at storage_class_aliases", err)
	}

	srv, p := newProviderEP(t, opts, s3bucket.Endpoint{StorageClassAliases: map[string]string{"COLD_TIER": "GLACIER", "WARM_TIER": "STANDARD_IA"}})
	converge(t, p, "fake/sc-bucket", want)
	if _, _, got, _, _ := srv.Snapshot("sc-bucket"); !strings.Contains(got, "COLD_TIER") || strings.Contains(got, "GLACIER") {
		t.Fatalf("the endpoint keeps %s; want the renamed storage class", got)
	}
	// 何周しても drift にならない。
	for i := 0; i < 3; i++ {
		pl, err := plan(t, p, "fake/sc-bucket", want)
		if err != nil || len(pl.Ops) != 0 {
			t.Fatalf("cycle %d: plan = %+v, %v; want converged", i, pl.Ops, err)
		}
	}
}
