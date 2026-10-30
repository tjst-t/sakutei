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
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket/s3test"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// lockEP は object_lock を扱う endpoint（fields に書く）。
var lockEP = s3bucket.Endpoint{Fields: []string{s3bucket.FieldVersioning, s3bucket.FieldObjectLock}}

func TestObjectLock_ParseAndCanon(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"{}", "{}"},
		{`{"enable_on_create": true}`, "{}"},
		{`{"mode": "COMPLIANCE", "days": 30}`, `{"days":30,"mode":"COMPLIANCE"}`},
		{`{"mode": "COMPLIANCE", "years": 7, "enable_on_create": false}`, `{"days":2555,"mode":"COMPLIANCE"}`},
	} {
		got, err := s3bucket.CanonObjectLock(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CanonObjectLock(%s) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{
		`[]`, `{"mode": "COMPLIANCE"}`, `{"days": 3}`, `{"mode": "COMPLIANCE", "days": 3, "years": 1}`,
		`{"mode": "LEGAL", "days": 3}`, `{"mode": "COMPLIANCE", "days": 0}`, `{"mode": "COMPLIANCE", "days": -1}`,
		`{"mode": "COMPLIANCE", "days": 1.5}`, `{"mode": "COMPLIANCE", "days": "3"}`, `{"mode": "COMPLIANCE", "years": 101}`,
		`{"enable_on_create": "yes"}`, `{"Mode": "COMPLIANCE", "days": 3}`,
	} {
		if _, err := s3bucket.ParseObjectLock(bad); err == nil {
			t.Errorf("ParseObjectLock(%s): no error", bad)
		}
	}
}

// object_lock は fields に書いたときだけ扱う：既定の endpoint では Get を呼ばず、
// 宣言すれば fields を案内するエラー。
func TestObjectLock_OptInThroughFields(t *testing.T) {
	srv, p := newProvider(t, s3test.Options{}, "")
	srv.CreateBucket("plain")
	obs, err := p.Read(context.Background(), "fake/plain")
	if err != nil || srv.Calls("GetObjectLockConfiguration") != 0 {
		t.Fatalf("Read = %v, GetObjectLockConfiguration calls %d; want none by default", err, srv.Calls("GetObjectLockConfiguration"))
	}
	if _, ok := obs.State[s3bucket.FieldObjectLock]; ok {
		t.Errorf("State has object_lock: %v", obs.State)
	}
	err = p.ValidateIntent("fake/plain", map[string]string{s3bucket.FieldObjectLock: "{}"})
	if err == nil || !strings.Contains(err.Error(), "listed in fields") {
		t.Errorf("ValidateIntent with object_lock on a default endpoint: %v", err)
	}
}

// 作るときの有効化を持たない実装（versioning は 501・COMPLIANCE だけ・Years を
// Days で返す）でも PutObjectLockConfiguration だけで宣言どおりになり、years の
// 宣言が Days の観測と合う。
func TestObjectLock_CreateWithoutCreateTimeEnable(t *testing.T) {
	srv, p := newProviderEP(t, s3test.Options{
		NoObjectLockOnCreate: true, NotImplemented: []string{s3test.APIVersioning},
		ObjectLockModes: []string{s3bucket.ObjectLockCompliance}, ObjectLockYearsAsDays: true,
	}, lockEP)
	want := map[string]string{s3bucket.FieldObjectLock: `{"enable_on_create": true, "mode": "COMPLIANCE", "years": 7}`}
	converge(t, p, "fake/vault", want)
	if mode, days, years, ok := srv.ObjectLock("vault"); !ok || mode != "COMPLIANCE" || days != 2555 || years != 0 {
		t.Errorf("object lock = %s %d %d %v", mode, days, years, ok)
	}
	if n := len(srv.LockPuts()); n != 1 {
		t.Errorf("PutObjectLockConfiguration %d times; want 1", n)
	}
	obs, _ := p.Read(context.Background(), "fake/vault")
	if got := obs.State[s3bucket.FieldObjectLock].Value; got != `{"days":2555,"mode":"COMPLIANCE"}` {
		t.Errorf("observed object_lock = %v", got)
	}
	if got := obs.State[s3bucket.FieldUnsupported].Value; got != "lifecycle,policy,tags,versioning" {
		t.Errorf("unsupported = %v; want versioning (501, not an error) and the settings outside fields", got)
	}
}

// 作るときの有効化を持つ実装では、有効化はヘッダで済み、既定の保持が無ければ
// PUT を送らない。
func TestObjectLock_CreateTimeEnable(t *testing.T) {
	srv, p := newProviderEP(t, s3test.Options{}, lockEP)
	converge(t, p, "fake/locked", map[string]string{s3bucket.FieldObjectLock: `{"enable_on_create": true}`})
	if _, _, _, ok := srv.ObjectLock("locked"); !ok || len(srv.LockPuts()) != 0 {
		t.Errorf("lock %v, puts %v; want enabled at create without a PUT", ok, srv.LockPuts())
	}
	converge(t, p, "fake/governed", map[string]string{s3bucket.FieldObjectLock: `{"mode": "GOVERNANCE", "days": 30}`})
	if mode, days, _, ok := srv.ObjectLock("governed"); !ok || mode != "GOVERNANCE" || days != 30 {
		t.Errorf("governed = %s %d %v", mode, days, ok)
	}
	// 宣言しない（"" でもない）なら比べない。
	srv.SetObjectLock("governed", "COMPLIANCE", 1, 0)
	if pl, err := plan(t, p, "fake/governed", map[string]string{}); err != nil || len(pl.Ops) != 0 {
		t.Errorf("undeclared object_lock: plan %v, %v", pl.Ops, err)
	}
}

// 受けない mode は理由つきの PermanentError で止まる（黙って流さない）。
func TestObjectLock_RefusedModeIsPermanent(t *testing.T) {
	srv, p := newProviderEP(t, s3test.Options{ObjectLockModes: []string{s3bucket.ObjectLockCompliance}}, lockEP)
	pl, err := plan(t, p, "fake/gov", map[string]string{s3bucket.FieldObjectLock: `{"mode": "GOVERNANCE", "days": 1}`})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Apply(context.Background(), pl)
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "refused the Object Lock configuration") || !strings.Contains(err.Error(), "InvalidArgument") {
		t.Fatalf("Apply = %v; want a PermanentError with the reason", err)
	}
	if _, _, _, ok := srv.ObjectLock("gov"); ok {
		t.Error("the refused configuration was kept")
	}
}

// API を持たない（501）実装：宣言していれば観測できないとして止め、Apply も
// NotImplemented の PermanentError。宣言していなければ無視。
func TestObjectLock_NotImplemented(t *testing.T) {
	srv, p := newProviderEP(t, s3test.Options{NotImplemented: []string{s3test.APIObjectLock}}, lockEP)
	srv.CreateBucket("old")
	if _, err := plan(t, p, "fake/old", map[string]string{s3bucket.FieldObjectLock: "{}"}); err == nil || !strings.Contains(err.Error(), "GetObjectLockConfiguration (NotImplemented)") {
		t.Errorf("plan with object_lock on an endpoint without the API: %v", err)
	}
	if pl, err := plan(t, p, "fake/old", map[string]string{}); err != nil || len(pl.Ops) != 0 {
		t.Errorf("undeclared: %v, %v", pl.Ops, err)
	}
	pl, err := plan(t, p, "fake/new", map[string]string{s3bucket.FieldObjectLock: `{"mode": "COMPLIANCE", "days": 1}`})
	if err != nil {
		t.Fatal(err)
	}
	var pe *provider.PermanentError
	if err := p.Apply(context.Background(), pl); !errors.As(err, &pe) || !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("Apply = %v; want a NotImplemented PermanentError", err)
	}
}

// 持ち主でない（403）は Unknown（「設定が無い」と読まない）。
func TestObjectLock_ForbiddenIsUnknown(t *testing.T) {
	srv, p := newProviderEP(t, s3test.Options{}, lockEP)
	srv.CreateBucket("theirs")
	srv.SetForeign("theirs")
	obs, err := p.Read(context.Background(), "fake/theirs")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Read = %+v, %v; want Unknown", obs, err)
	}
}

// 既にあるバケットと食い違う宣言（外す・作るときの有効化）は計画で止まる。
func TestObjectLock_ConflictsStop(t *testing.T) {
	srv, p := newProviderEP(t, s3test.Options{}, lockEP)
	srv.CreateBucket("locked")
	srv.SetObjectLock("locked", "", 0, 0)
	srv.CreateBucket("plain")
	if _, err := plan(t, p, "fake/locked", map[string]string{s3bucket.FieldObjectLock: ""}); err == nil || !strings.Contains(err.Error(), "cannot be turned off") {
		t.Errorf("declaring no Object Lock on a locked bucket: %v", err)
	}
	if _, err := plan(t, p, "fake/plain", map[string]string{s3bucket.FieldObjectLock: `{"enable_on_create": true}`}); err == nil || !strings.Contains(err.Error(), "does not re-create") {
		t.Errorf("enable_on_create on an existing bucket without Object Lock: %v", err)
	}
	if pl, err := plan(t, p, "fake/plain", map[string]string{s3bucket.FieldObjectLock: ""}); err != nil || len(pl.Ops) != 0 {
		t.Errorf("no Object Lock declared on a plain bucket: %v, %v", pl.Ops, err)
	}
}

// 既にあるバケットの保持の設定を変える Update は向きによらず承認が要り、作成は
// 要らない。
func TestObjectLock_ApprovalRequired(t *testing.T) {
	_, p := newProviderEP(t, s3test.Options{}, lockEP)
	upd := func(from, to string) string {
		op := core.Op{Kind: core.OpKindUpdate, Fields: []core.FieldDiff{{Field: s3bucket.FieldObjectLock, From: from, To: to}}}
		return p.ApprovalRequired("fake/b", core.State{}, map[string]string{s3bucket.FieldObjectLock: to}, op)
	}
	c30 := `{"days":30,"mode":"COMPLIANCE"}`
	for _, tc := range []struct{ from, to, want string }{
		{"", `{}`, "turns Object Lock on"},
		{"{}", `{"mode":"COMPLIANCE","days":30}`, "sets the default retention COMPLIANCE 30 days"},
		{c30, `{"mode":"COMPLIANCE","days":60}`, "lengthens the default retention 30 → 60 days"},
		{c30, `{"mode":"COMPLIANCE","days":1}`, "shortens the default retention 30 → 1 days"},
		{c30, `{}`, "removes the default retention"},
		{`{"days":30,"mode":"GOVERNANCE"}`, `{"mode":"COMPLIANCE","days":30}`, "changes the default retention mode GOVERNANCE → COMPLIANCE"},
		{c30, `{"mode":"GOVERNANCE","days":30}`, "changes the default retention mode COMPLIANCE → GOVERNANCE"},
	} {
		if got := upd(tc.from, tc.to); !strings.Contains(got, tc.want) {
			t.Errorf("%s → %s: reason %q; want it to say %q", tc.from, tc.to, got, tc.want)
		}
	}
	create := core.Op{Kind: core.OpKindCreate, Fields: []core.FieldDiff{{Field: s3bucket.FieldObjectLock, To: c30}}}
	if got := p.ApprovalRequired("fake/b", nil, map[string]string{s3bucket.FieldObjectLock: c30}, create); got != "" {
		t.Errorf("Create: %q; want no approval", got)
	}
	tagsOnly := core.Op{Kind: core.OpKindUpdate, Fields: []core.FieldDiff{{Field: s3bucket.FieldTags, To: "{}"}}}
	if got := p.ApprovalRequired("fake/b", nil, map[string]string{s3bucket.FieldObjectLock: c30}, tagsOnly); got != "" {
		t.Errorf("an Update without object_lock: %q", got)
	}
}
