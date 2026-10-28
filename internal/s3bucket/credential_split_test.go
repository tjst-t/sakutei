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

// credential_split_test.go — 観測用と書き込み用のキー（§20.7、#186）。

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket/s3test"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	roKey    = "ro-access"
	roSecret = "ro-secret"
	// splitStore は観測用（読むだけ）と書き込み用のキーの両方を持つ store。
	splitStore = `{"secrets": {"s3/ro_ak": {"value": "ro-access"}, "s3/ro_sk": {"value": "ro-secret"},
  "s3/rw_ak": {"value": "test-access"}, "s3/rw_sk": {"value": "test-secret"}}}`
	// roOnlyStore は観測用のキーだけを持つ store。
	roOnlyStore = `{"secrets": {"s3/ro_ak": {"value": "ro-access"}, "s3/ro_sk": {"value": "ro-secret"}}}`
)

// splitRig は読むだけのキーと書き込み用のキーを受ける偽の S3 と、その 2 組を
// observe:・write: に書いた endpoint の一覧（path）を返す。
func splitRig(t *testing.T, store string) (*s3test.Server, string) {
	t.Helper()
	srv := s3test.New(s3test.Options{ReadOnlyAccessKey: roKey, ReadOnlySecretKey: roSecret})
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "secret_store: secrets.json\nendpoints:\n  fake:\n    endpoint: " + srv.URL + "\n" +
		"    observe:\n      access_key: secretref:file/s3/ro_ak\n      secret_key: secretref:file/s3/ro_sk\n" +
		"    write:\n      access_key: secretref:file/s3/rw_ak\n      secret_key: secretref:file/s3/rw_sk\n"
	path := filepath.Join(dir, "s3.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, path
}

func loadSplit(path string, opts s3bucket.LoadOptions) (*s3bucket.Provider, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return nil, err
	}
	return s3bucket.NewFromConfigTreeWith(path, tree, opts)
}

func splitWant() map[string]string {
	return map[string]string{
		s3bucket.FieldVersioning: "Enabled", s3bucket.FieldPolicy: samplePolicy,
		s3bucket.FieldLifecycle: sampleLifecycle, s3bucket.FieldTags: sampleTags,
	}
}

func splitPlan(t *testing.T, p *s3bucket.Provider, id string) core.Plan {
	t.Helper()
	obs, err := p.Read(context.Background(), id)
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read with the observe key: %v %+v", err, obs)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: core.NewID(s3bucket.Name, core.NewScope(nil), id), Fields: splitWant()},
		cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func TestCredentialSplit_ObserveWithReadOnlyKeyWriteWithWriteKey(t *testing.T) {
	// [AC-186-6-1]
	srv, path := splitRig(t, splitStore)
	p, err := loadSplit(path, s3bucket.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const id = "fake/sakutei-contract"
	for i := 0; i < 5; i++ {
		pl := splitPlan(t, p, id)
		if len(pl.Ops) == 0 {
			break
		}
		if err := p.Apply(provider.WithWrite(context.Background()), pl); err != nil {
			t.Fatalf("Apply with the write key: %v", err)
		}
	}
	if pl := splitPlan(t, p, id); len(pl.Ops) != 0 {
		t.Fatalf("not converged: %+v", pl.Ops)
	}
	if _, err := p.List(context.Background(), core.NewScope(nil)); err != nil {
		t.Fatalf("List with the observe key: %v", err)
	}
	if _, err := p.BulkObserve(context.Background(), core.NewScope(nil)); err != nil {
		t.Fatalf("BulkObserve with the observe key: %v", err)
	}
	ro, rw := srv.KeyCalls(roKey), srv.KeyCalls("test-access")
	if ro["READ"] == 0 || ro["WRITE"] != 0 {
		t.Errorf("the read-only key: %v; want reads only", ro)
	}
	if rw["WRITE"] == 0 {
		t.Errorf("the write key: %v; want the writes", rw)
	}
	// Apply の中の読み戻しは書き込み用のキー。観測は書き込み用のキーを使わない:
	// 収束した後の Read・List・BulkObserve で書き込み用のキーの回数は増えない。
	before := srv.KeyCalls("test-access")
	splitPlan(t, p, id)
	if after := srv.KeyCalls("test-access"); after["READ"] != before["READ"] || after["WRITE"] != before["WRITE"] {
		t.Errorf("an observation used the write key: %v → %v", before, after)
	}
}

func TestCredentialSplit_ObserveOnlyRunNeedsNoWriteKey(t *testing.T) {
	// [AC-186-6-2]
	srv, path := splitRig(t, roOnlyStore)
	// 書く回は書き込み用のキーが store に無ければ起動しない。
	if _, err := loadSplit(path, s3bucket.LoadOptions{}); err == nil ||
		!strings.Contains(err.Error(), "endpoints.fake.write.access_key") {
		t.Fatalf("a writing run without the write keys: %v", err)
	}
	p, err := loadSplit(path, s3bucket.LoadOptions{ObserveOnly: true})
	if err != nil {
		t.Fatalf("an observe-only run: %v", err)
	}
	pl := splitPlan(t, p, "fake/sakutei-new")
	if len(pl.Ops) == 0 {
		t.Fatal("want a plan to create the bucket")
	}
	err = p.Apply(provider.WithWrite(context.Background()), pl)
	if !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Fatalf("Apply in an observe-only run: %v", err)
	}
	if w := srv.KeyCalls("test-access"); len(w) != 0 {
		t.Errorf("the write key was used: %v", w)
	}
	if ro := srv.KeyCalls(roKey); ro["WRITE"] != 0 {
		t.Errorf("the read-only key tried to write: %v", ro)
	}
	if srv.Has("sakutei-new") {
		t.Error("the bucket was created in an observe-only run")
	}
}

func TestCredentialSplit_ReadOnlyKeyCannotWrite(t *testing.T) {
	// [AC-186-6-3] 書き込み用に読むだけのキーを書いた誤り: endpoint が断り
	// （403、PermanentError）、何も変わらない。
	srv := s3test.New(s3test.Options{ReadOnlyAccessKey: roKey, ReadOnlySecretKey: roSecret})
	t.Cleanup(srv.Close)
	c, err := s3bucket.NewClient(s3bucket.ClientConfig{
		Endpoint: srv.URL, Credentials: staticCreds(roKey, roSecret), WriteCredentials: staticCreds(roKey, roSecret),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := s3bucket.New(map[string]s3bucket.Endpoint{"fake": {Client: c}}, s3bucket.Options{})
	pl := splitPlan(t, p, "fake/sakutei-new")
	err = p.Apply(provider.WithWrite(context.Background()), pl)
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || s3bucket.KindOf(err) != s3bucket.KindAuth {
		t.Fatalf("Apply with the read-only key = %v; want a PermanentError of KindAuth", err)
	}
	if srv.Has("sakutei-new") {
		t.Error("the bucket was created with the read-only key")
	}
}

func TestCredentialSplit_ConfigErrors(t *testing.T) {
	// [AC-186-6-4]
	base := "secret_store: secrets.json\nendpoints:\n  fake:\n    endpoint: http://127.0.0.1:9000\n"
	both := "    access_key: secretref:file/s3/ro_ak\n    secret_key: secretref:file/s3/ro_sk\n"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"observe is not a map", both + "    observe: secretref:file/s3/ro_ak\n", "endpoints.fake.observe: must be a map"},
		{"unknown key in write", both + "    write:\n      region: us-east-1\n", `unknown key "endpoints.fake.write.region"`},
		{"write without keys anywhere", "    observe:\n      access_key: secretref:file/s3/ro_ak\n      secret_key: secretref:file/s3/ro_sk\n    write:\n      access_key: secretref:file/s3/rw_ak\n",
			"endpoints.fake.write.secret_key is required"},
		{"raw key in observe", both + "    observe:\n      secret_key: plain-text-key\n", "endpoints.fake.observe.secret_key: must be a SecretRef"},
		{"write key not in the store", both + "    write:\n      secret_key: secretref:file/s3/missing\n", "endpoints.fake.write.secret_key: secretref:file/s3/missing"},
		{"observe key not in the store", both + "    observe:\n      access_key: secretref:file/s3/missing\n", "endpoints.fake.observe.access_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(splitStore), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "s3.yaml")
			if err := os.WriteFile(path, []byte(base+tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := s3bucket.LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "plain-text-key") {
				t.Errorf("the raw key is in the error: %v", err)
			}
		})
	}
	// 書かないキーは endpoint の上の値を受け継ぐ。observe-only の回は write の
	// store を確かめない。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(roOnlyStore), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s3.yaml")
	body := base + both + "    write:\n      secret_key: secretref:file/s3/rw_sk\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := s3bucket.LoadConfigWith(path, s3bucket.LoadOptions{ObserveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Endpoints["fake"]
	if e.WriteAccessKey.Path() != "s3/ro_ak" || e.WriteSecretKey.Path() != "s3/rw_sk" || e.AccessKey.Path() != "s3/ro_ak" || e.SecretKey.Path() != "s3/ro_sk" {
		t.Errorf("keys = %+v", e)
	}
	if _, err := s3bucket.LoadConfig(path); err == nil || !strings.Contains(err.Error(), "endpoints.fake.write.secret_key") {
		t.Errorf("a writing run without the write secret_key: %v", err)
	}
}

func TestAccessDeniedOnGetIsUnknownNotAbsent(t *testing.T) {
	// [AC-186-6-5] 読むだけのポリシーに Get の許可が足りない（403 AccessDenied）
	// のを「設定が無い」と読まない（§3 第3原則）: 観測できない（Unknown）。
	for _, api := range []string{s3test.APIVersioning, s3test.APIPolicy, s3test.APILifecycle, s3test.APITagging} {
		t.Run(api, func(t *testing.T) {
			srv, p := newProvider(t, s3test.Options{GetStatus: map[string]int{api: http.StatusForbidden}}, "")
			srv.CreateBucket("sakutei-denied")
			obs, err := p.Read(context.Background(), "fake/sakutei-denied")
			if err == nil || obs.Availability != core.AvailabilityUnknown || obs.State != nil {
				t.Fatalf("Read = %+v, %v; want unknown", obs, err)
			}
			bulk, err := p.BulkObserve(context.Background(), core.NewScope(nil))
			if err != nil {
				t.Fatal(err)
			}
			key := core.NewID(s3bucket.Name, core.NewScope(nil), "fake/sakutei-denied").Key()
			if o, ok := bulk.Observations[key]; !ok || o.Availability != core.AvailabilityUnknown {
				t.Fatalf("BulkObserve = %+v; want unknown", o)
			}
		})
	}
}
