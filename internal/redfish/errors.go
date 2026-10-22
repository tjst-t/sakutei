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

package redfish

import (
	"errors"
	"fmt"
)

// ErrorKind は Client が返すエラーの分類。provider はこれで Observe の
// Availability と Apply のエラー型を決める（§29.1 の（3））。
type ErrorKind int

const (
	// KindUnreachable — BMC に届かない・タイムアウト・5xx・認証以外の想定外の
	// 応答（3xx、202、読めない本文など）。provider は Unknown にし、何も操作
	// しない（unknown ≠ absent）。
	KindUnreachable ErrorKind = iota + 1
	// KindAuth — 401 / 403。設定（資格）を直さない限り治らないので、provider は
	// PermanentError にできる。
	KindAuth
	// KindCertificate — サーバ証明書を検証できない（自己署名を明示していない、
	// CA ファイルが違う、ホスト名が合わない）。KindAuth と同じく設定を直さない
	// 限り治らない。
	KindCertificate
	// KindRequest — 401 / 403 以外の 4xx（400・404・405 など）。BMC が要求を
	// 拒んだので、Message に BMC が返した内容を入れる。
	KindRequest
)

func (k ErrorKind) String() string {
	switch k {
	case KindUnreachable:
		return "unreachable"
	case KindAuth:
		return "authentication failed"
	case KindCertificate:
		return "certificate rejected"
	case KindRequest:
		return "request rejected"
	}
	return "unknown"
}

// Error は Client の全エラーの型。errors.As で取り出すか、IsUnreachable などで
// 分類を見る。
type Error struct {
	Kind   ErrorKind
	Method string
	Path   string
	// StatusCode は HTTP の応答があったときの状態コード（無ければ 0）。
	StatusCode int
	// Message は BMC が返したエラーの内容（Redfish の error.message と
	// @Message.ExtendedInfo、読めなければ本文の先頭）か、こちらで見つけた
	// 想定外の内容。
	Message string
	// Err は下の原因（net/http・TLS・JSON のエラー）。無ければ nil。
	Err error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("redfish: %s %s: %s", e.Method, e.Path, e.Kind)
	if e.StatusCode != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

func kindOf(err error) ErrorKind {
	var re *Error
	if errors.As(err, &re) {
		return re.Kind
	}
	return 0
}

// IsUnreachable は err が「BMC に届かない」（KindUnreachable）かを返す。
func IsUnreachable(err error) bool { return kindOf(err) == KindUnreachable }

// IsAuth は err が認証失敗（401 / 403）かを返す。
func IsAuth(err error) bool { return kindOf(err) == KindAuth }

// IsCertificate は err がサーバ証明書の検証失敗かを返す。
func IsCertificate(err error) bool { return kindOf(err) == KindCertificate }

// IsRequest は err が BMC に拒まれた要求（401 / 403 以外の 4xx）かを返す。
func IsRequest(err error) bool { return kindOf(err) == KindRequest }

// IsPermanent は err が設定を直さない限り治らないもの（認証失敗・証明書の
// 検証失敗）かを返す。provider はこれを provider.PermanentError にする。
func IsPermanent(err error) bool {
	k := kindOf(err)
	return k == KindAuth || k == KindCertificate
}
