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

// Package approve は承認ツール sakutei-approve の中身（設計 §5.4・§7.2、E-M4・
// N-M4・E-L3）。ネットワーク越しの承認と拒否を、CLI の approve・reject と同じ形の
// 判断のファイル（approvals/<id>/by-<承認者>.json）として Record に書くだけ。
// 認証はしない：承認者は前段の認証の結果（確かめたクライアント証明書の CN か、
// 認証をする前段の proxy が付けるヘッダ）から受け取る。数えて効かせ、監査に
// 残すのは周（不変条件 21）。申請者と要件に無い人の判断・待っていない依頼への
// 判断は書く前に断る（approval.Decide）。
//
//	GET  /approvals                 待っている依頼の一覧
//	GET  /approvals/{id}            依頼 1 つと数え方
//	POST /approvals/{id}/approve    {"comment": "…"}
//	POST /approvals/{id}/reject     {"comment": "…"}（理由は必須）
package approve

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/config"
	"github.com/tjst-t/sakutei/v3/record"
)

// Server は承認の HTTP。
type Server struct {
	// RecordDir は Record のディレクトリ（判断を書く・依頼を読む。ロックは取らない）。
	RecordDir string
	// UserHeader を書けば承認者をこのヘッダから受け取る（認証をする前段の proxy が
	// 付ける）。空ならクライアント証明書の CN。
	UserHeader string
	// Now は判断の時刻（nil なら計算機の時計）。
	Now func() time.Time
}

// Listen は承認の待ち受けの設定。
type Listen struct {
	Address string
	// TLSCert・TLSKey はサーバの証明書と鍵。ClientCA を書けばクライアント証明書を
	// 求めて確かめる。
	TLSCert, TLSKey, ClientCA string
	// UserHeader は Server.UserHeader と同じ。
	UserHeader string
	// Plaintext は loopback でない宛先を TLS なしで待ち受けることの明示。
	Plaintext bool
}

// Check は待ち受けの設定を確かめる。承認者を名乗れる経路を前段の認証だけに
// 絞る：ヘッダから受け取るなら、loopback（同じ計算機の proxy）かクライアント
// 証明書（proxy の証明書）を求める待ち受けだけ。ヘッダを使わないならクライアント
// 証明書が要る。
func (l Listen) Check() error {
	tlsOn := l.TLSCert != "" || l.TLSKey != ""
	switch {
	case l.Address == "":
		return errors.New("--listen に待ち受けの宛先（host:port）を書く")
	case (l.TLSCert == "") != (l.TLSKey == ""):
		return errors.New("--tls-cert と --tls-key を両方書く")
	case l.ClientCA != "" && !tlsOn:
		return errors.New("--tls-client-ca は --tls-cert と --tls-key と一緒に書く")
	case tlsOn && l.Plaintext:
		return errors.New("TLS と --plaintext は一緒に書けない")
	case !tlsOn && !l.Plaintext && !config.Loopback(l.Address):
		return fmt.Errorf("loopback でない宛先 %q を TLS なしで待ち受けるには --plaintext を書く", l.Address)
	case l.UserHeader == "" && l.ClientCA == "":
		return errors.New("承認者を受け取る口が無い：--tls-client-ca（クライアント証明書の CN）か --user-header（前段の proxy のヘッダ）を書く")
	case l.UserHeader != "" && l.ClientCA == "" && !config.Loopback(l.Address):
		return fmt.Errorf("--user-header は loopback の待ち受けか --tls-client-ca と一緒に使う（%q では誰でもヘッダを名乗れる）", l.Address)
	}
	return nil
}

// Open は待ち受けを作る。
func (l Listen) Open(h http.Handler) (*http.Server, net.Listener, error) {
	if err := l.Check(); err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", l.Address)
	if err != nil {
		return nil, nil, err
	}
	if l.TLSCert == "" {
		return srv, ln, nil
	}
	cert, err := tls.LoadX509KeyPair(l.TLSCert, l.TLSKey)
	if err != nil {
		ln.Close()
		return nil, nil, fmt.Errorf("--tls-cert: %w", err)
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if l.ClientCA != "" {
		pem, err := os.ReadFile(l.ClientCA)
		if err != nil {
			ln.Close()
			return nil, nil, fmt.Errorf("--tls-client-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			ln.Close()
			return nil, nil, errors.New("--tls-client-ca: 証明書が読めない")
		}
		tc.ClientCAs, tc.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	srv.TLSConfig = tc
	return srv, tls.NewListener(ln, tc), nil
}

// Handler は承認の HTTP の口。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /approvals", s.list)
	mux.HandleFunc("GET /approvals/{id}", s.get)
	mux.HandleFunc("POST /approvals/{id}/approve", func(w http.ResponseWriter, r *http.Request) { s.decide(w, r, record.Approve) })
	mux.HandleFunc("POST /approvals/{id}/reject", func(w http.ResponseWriter, r *http.Request) { s.decide(w, r, record.Reject) })
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// who は前段の認証の結果から承認者を受け取る。
func (s *Server) who(r *http.Request) (string, error) {
	var who string
	if s.UserHeader != "" {
		who = strings.TrimSpace(r.Header.Get(s.UserHeader))
	} else if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		who = r.TLS.VerifiedChains[0][0].Subject.CommonName
	}
	if who == "" {
		return "", errors.New("承認者が分からない（前段の認証の結果が無い）")
	}
	if !record.ValidName(who) {
		return "", fmt.Errorf("承認者の名前 %q に使えない文字がある（英数字と . _ @ -）", who)
	}
	return who, nil
}

// Item は依頼 1 つ（一覧と GET の返すもの）。
type Item struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	Subject    string        `json:"subject"`
	Applicant  string        `json:"applicant,omitempty"`
	Rules      []record.Rule `json:"rules"`
	Expires    *time.Time    `json:"expires,omitempty"`
	State      string        `json:"state"`
	Approvers  []string      `json:"approvers,omitempty"`
	RejectedBy []string      `json:"rejected_by,omitempty"`
	Progress   []string      `json:"progress,omitempty"`
	Ignored    []string      `json:"ignored,omitempty"`
}

func item(id string, a record.Approval, st approval.Status) Item {
	req := a.Request
	return Item{ID: id, Kind: req.Kind, Subject: req.Subject, Applicant: req.Applicant, Rules: req.Rules, Expires: req.Expires,
		State: st.State, Approvers: st.Approvers, RejectedBy: st.RejectedBy, Progress: st.Progress, Ignored: st.Ignored}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	all, err := record.ReadApprovals(s.RecordDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := s.now()
	out := []Item{}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st := approval.Evaluate(all[id], true, now)
		if st.State == approval.Pending || r.URL.Query().Get("all") != "" {
			out = append(out, item(id, all[id], st))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok, err := record.ReadApproval(s.RecordDir, id)
	switch {
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	case !ok:
		writeError(w, http.StatusNotFound, fmt.Sprintf("依頼 %s が無い", id))
	default:
		writeJSON(w, http.StatusOK, item(id, a, approval.Evaluate(a, true, s.now())))
	}
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, decision string) {
	who, err := s.who(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Comment string `json:"comment"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err == nil && len(bytes.TrimSpace(data)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		err = dec.Decode(&body)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "本文は {\"comment\": \"…\"} の JSON")
		return
	}
	id := r.PathValue("id")
	st, err := approval.Decide(s.RecordDir, id, record.Decision{By: who, Decision: decision, Comment: body.Comment, At: s.now()})
	if err != nil {
		code := http.StatusInternalServerError
		var de *approval.DecideError
		if errors.As(err, &de) {
			code = map[string]int{approval.DecideInvalid: http.StatusBadRequest, approval.DecideNotFound: http.StatusNotFound,
				approval.DecideClosed: http.StatusConflict, approval.DecideForbidden: http.StatusForbidden}[de.Code]
		}
		writeError(w, code, err.Error())
		return
	}
	a, _, _ := record.ReadApproval(s.RecordDir, id)
	writeJSON(w, http.StatusOK, struct {
		Decision string `json:"decision"`
		By       string `json:"by"`
		Item
		Note string `json:"note"`
	}{decision, who, item(id, a, st), "数えて効かせるのは次の周"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
