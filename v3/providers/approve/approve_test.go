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

package approve_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/providers/approve"
	"github.com/tjst-t/sakutei/v3/providers/internal/testcert"
	"github.com/tjst-t/sakutei/v3/record"
)

var now = time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)

func setup(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "record")
	rec, err := record.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	exp := now.Add(time.Hour)
	for _, req := range []record.ApprovalRequest{
		{ID: "chg-1", Kind: "change", Subject: "dns/record/www", Applicant: "carol",
			Rules: []record.Rule{{From: "platform", N: 2, Of: []string{"alice", "bob", "carol"}}}, CreatedAt: now, Expires: &exp, State: record.RequestPending},
		{ID: "chg-used", Kind: "change", Subject: "dns/record/api", Applicant: "carol",
			Rules: []record.Rule{{From: "platform", N: 1}}, CreatedAt: now, State: record.RequestUsed},
	} {
		if err := rec.WriteRequest(req, false); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func post(t *testing.T, c *http.Client, url, user, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if user != "" {
		req.Header.Set("X-Remote-User", user)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// 不変条件 21・E-M4：申請者と承認者が同じなら拒否し、判断のファイルを書かない。
// 要件に無い人・待っていない依頼・名乗りの無い要求も拒否する。数える人の判断は CLI と
// 同じ形のファイルに書き、数え方を返す。
func TestApproveHeader(t *testing.T) {
	dir := setup(t)
	s := &approve.Server{RecordDir: dir, UserHeader: "X-Remote-User", Now: func() time.Time { return now }}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	c := srv.Client()
	u := srv.URL + "/approvals/chg-1/approve"

	for _, x := range []struct {
		name, url, user, body string
		code                  int
		want                  string
	}{
		{"申請者", u, "carol", "", http.StatusForbidden, "申請者≠承認者"},
		{"要件に無い人", u, "mallory", "", http.StatusForbidden, "承認者（platform: 2 人（alice, bob, carol））に無い"},
		{"名乗りが無い", u, "", "", http.StatusUnauthorized, "承認者が分からない"},
		{"使えない名前", u, "../bob", "", http.StatusUnauthorized, "使えない文字"},
		{"使った依頼", srv.URL + "/approvals/chg-used/approve", "alice", "", http.StatusConflict, "used"},
		{"無い依頼", srv.URL + "/approvals/chg-9/approve", "alice", "", http.StatusNotFound, "依頼 chg-9 が無い"},
		{"理由の無い拒否", srv.URL + "/approvals/chg-1/reject", "bob", `{}`, http.StatusBadRequest, "拒否の理由"},
		{"知らない本文", u, "alice", `{"by": "carol"}`, http.StatusBadRequest, "comment"},
	} {
		code, out := post(t, c, x.url, x.user, x.body)
		if code != x.code || !strings.Contains(out["error"].(string), x.want) {
			t.Errorf("%s: %d %v（%d・%q のはず）", x.name, code, out, x.code, x.want)
		}
	}
	if files, _ := os.ReadDir(filepath.Join(dir, "approvals", "chg-1")); len(files) != 1 {
		t.Fatalf("拒否した判断を書いた: %d ファイル", len(files))
	}

	code, out := post(t, c, u, "alice", `{"comment": "確認済み"}`)
	if code != 200 || out["state"] != "pending" || out["by"] != "alice" {
		t.Fatalf("alice: %d %v", code, out)
	}
	a, _, _ := record.ReadApproval(dir, "chg-1")
	if len(a.Decisions) != 1 || a.Decisions[0].By != "alice" || a.Decisions[0].Comment != "確認済み" || !a.Decisions[0].At.Equal(now) {
		t.Fatalf("判断のファイル: %+v", a.Decisions)
	}
	if code, out := post(t, c, u, "bob", ""); code != 200 || out["state"] != "approved" {
		t.Fatalf("bob: %d %v", code, out)
	}

	resp, err := c.Get(srv.URL + "/approvals")
	if err != nil {
		t.Fatal(err)
	}
	var list []approve.Item
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("待っている依頼は無い: %+v", list)
	}
	resp, _ = c.Get(srv.URL + "/approvals?all=1")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 2 || list[0].ID != "chg-1" || list[0].State != "approved" {
		t.Fatalf("全部: %+v", list)
	}
}

// クライアント証明書の CN を承認者にする（前段の認証）。証明書の無い接続は拒否する。
func TestApproveClientCert(t *testing.T) {
	dir := setup(t)
	work := t.TempDir()
	f := testcert.Write(t, work)
	ca, caKey, caPEM := testcert.CA(t)
	clientCA := filepath.Join(work, "client-ca.pem")
	os.WriteFile(clientCA, caPEM, 0o600)
	l := approve.Listen{Address: "127.0.0.1:0", TLSCert: f.ServerCert, TLSKey: f.ServerKey, ClientCA: clientCA}
	s := &approve.Server{RecordDir: dir, Now: func() time.Time { return now }}
	srv, ln, err := l.Open(s.Handler())
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	roots := x509.NewCertPool()
	pem, _ := os.ReadFile(f.CA)
	roots.AppendCertsFromPEM(pem)
	client := func(cn string) *http.Client {
		tc := &tls.Config{RootCAs: roots}
		if cn != "" {
			der, key := testcert.Sign(t, ca, caKey, cn, x509.ExtKeyUsageClientAuth)
			cert, err := tls.X509KeyPair(testcert.PEM("CERTIFICATE", der), testcert.PEM("EC PRIVATE KEY", key))
			if err != nil {
				t.Fatal(err)
			}
			tc.Certificates = []tls.Certificate{cert}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}
	}
	u := "https://" + ln.Addr().String() + "/approvals/chg-1/approve"
	if code, out := post(t, client("carol"), u, "", ""); code != http.StatusForbidden {
		t.Fatalf("申請者の証明書: %d %v", code, out)
	}
	if code, out := post(t, client("bob"), u, "alice", ""); code != 200 || out["by"] != "bob" {
		t.Fatalf("CN が承認者（ヘッダは使わない）: %d %v", code, out)
	}
	if _, err := client("").Post(u, "application/json", bytes.NewReader(nil)); err == nil {
		t.Fatal("証明書の無い接続を受け付けた")
	}
}

func TestListenCheck(t *testing.T) {
	for _, c := range []struct {
		l    approve.Listen
		want string
	}{
		{approve.Listen{}, "--listen"},
		{approve.Listen{Address: "127.0.0.1:8444"}, "承認者を受け取る経路が無い"},
		{approve.Listen{Address: "0.0.0.0:8444", UserHeader: "X-Remote-User"}, "--plaintext"},
		{approve.Listen{Address: "0.0.0.0:8444", UserHeader: "X-Remote-User", Plaintext: true}, "誰でもヘッダを名乗れる"},
		{approve.Listen{Address: "0.0.0.0:8444", UserHeader: "X-Remote-User", TLSCert: "a", TLSKey: "b"}, "誰でもヘッダを名乗れる"},
		{approve.Listen{Address: "127.0.0.1:8444", ClientCA: "ca"}, "--tls-client-ca は"},
	} {
		if err := c.l.Check(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v（%q を含むはず）", c.l, err, c.want)
		}
	}
	for _, l := range []approve.Listen{
		{Address: "127.0.0.1:8444", UserHeader: "X-Remote-User"},
		{Address: "0.0.0.0:8444", UserHeader: "X-Remote-User", TLSCert: "a", TLSKey: "b", ClientCA: "c"},
		{Address: "0.0.0.0:8444", TLSCert: "a", TLSKey: "b", ClientCA: "c"},
	} {
		if err := l.Check(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
}
