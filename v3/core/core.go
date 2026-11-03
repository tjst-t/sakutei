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

// Package core は新しいコアの正準型（Resource・Range・Schema・Observation と
// Provider のインターフェース）を持つ（設計 docs/core/README.md §3.1〜§3.3）。
//
// 項目に付けた E-…・N-… は docs/review/core-usecase-review.md のユースケース ID。
// ID を付けられない項目は入れない。
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ID は Resource の識別子。Type は "<provider のインスタンス名>/<種類>"、Name は
// その種類の中の名前。intent 全体で一意（E-M3・N-M3）。
type ID struct {
	Type string
	Name string
}

// String は "<provider>/<kind>/<name>" を返す。
func (id ID) String() string { return id.Type + "/" + id.Name }

// Provider は Type の provider のインスタンス名を返す。
func (id ID) Provider() string {
	p, _, _ := strings.Cut(id.Type, "/")
	return p
}

// Kind は Type の種類を返す。
func (id ID) Kind() string {
	_, k, _ := strings.Cut(id.Type, "/")
	return k
}

// Local は provider の中での ID（"<kind>/<name>"）。Range の除外が突き合わせる（E-D2）。
func (id ID) Local() string { return id.Kind() + "/" + id.Name }

// ParseType は "<provider>/<kind>" を確かめる。種類に "/" は含めない。
func ParseType(s string) error {
	p, k, ok := strings.Cut(s, "/")
	if !ok || p == "" || k == "" || strings.Contains(k, "/") {
		return fmt.Errorf("type %q は <provider>/<種類> の形でない", s)
	}
	return nil
}

// ParseRef は "<provider>/<kind>/<name>" を ID にする（depends_on の参照、E-J7）。
func ParseRef(s string) (ID, error) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ID{}, fmt.Errorf("参照 %q は <provider>/<種類>/<名前> の形でない", s)
	}
	return ID{Type: parts[0] + "/" + parts[1], Name: parts[2]}, nil
}

// Fields は宣言した項目だけを持つ（E-D7・E-B7）。観測の Fields は正規形。
type Fields map[string]any

// Ensure は在るべきか無いべきか。
type Ensure int

const (
	// EnsurePresent は在るべき（既定）。
	EnsurePresent Ensure = iota
	// EnsureAbsent は名指しで消す（E-D3・N-A9）。
	EnsureAbsent
)

func (e Ensure) String() string {
	if e == EnsureAbsent {
		return "absent"
	}
	return "present"
}

// Origin は宣言のファイルと行（E-M3：断るときにどこかを示す）。
type Origin struct {
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
}

func (o Origin) String() string {
	if o.File == "" {
		return ""
	}
	if o.Line == 0 {
		return o.File
	}
	return fmt.Sprintf("%s:%d", o.File, o.Line)
}

// Dependency は順序と待ち（§3.5、E-J7・E-B13・E-I5）。On は名指しの上流（無くて
// もよい。When だけの待ち、E-C4）。When があれば、真になるまで下流は操作しない
// （E-C4・N-C8・N-J5・N-H6）。When が無ければ、宣言した上流は宣言どおり、宣言に
// 無い上流は Present になるまで待つ。
type Dependency struct {
	On        ID
	When      Condition
	OnUnknown Disposition
}

// HasOn は名指しの上流があるか。
func (d Dependency) HasOn() bool { return d.On != ID{} }

// Resource は宣言 1 件（§3.1）。
type Resource struct {
	ID        ID
	Ensure    Ensure            // E-D3・N-A9
	Fields    Fields            // E-D7・E-B7・N-I2・E-B14・E-A10
	Labels    map[string]string // E-M1・E-M2・N-M5
	DependsOn []Dependency      // E-J7・E-B13・E-I5
	Apply     *ApplyOptions     // E-B3・E-B8・N-B2・N-D5
	Reason    string            // N-J7・N-L8
	Origin    Origin            // E-M3
}

// Range は「この provider のこの集合はこちらが管理する」境界（§3.2）。
// 範囲は intent にだけ書く（E-I6・不変条件 24）。
type Range struct {
	Provider       string            // E-B2・E-I6・E-G1・E-B4
	Args           map[string]string // List の引数（E-B2・E-I6・E-G1・E-B4）
	Labels         map[string]string // E-D4・N-I3
	Prune          bool              // 既定は消さない（E-C3・N-C2・N-F3・N-T1）
	MaxDrop        int               // 百分率。既定 50（E-K2・N-K2）
	ExternalOwners []Exclusion       // E-D2・E-I5・E-T2・N-T2・E-O1・N-O3
	Origin         Origin
}

// Key は範囲の名前（"<provider>:<引数=値,…>"）。Record の目印の範囲ごとの件数の
// 鍵にする（E-K2）。
func (r Range) Key() string {
	keys := make([]string, 0, len(r.Args))
	for k := range r.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + r.Args[k]
	}
	return r.Provider + ":" + strings.Join(parts, ",")
}

// DefaultMaxDrop は max_drop を書かないときの値（E-K2）。
const DefaultMaxDrop = 50

// Exclusion は外の持ち主のもの（触らない）。ID か IDPrefix のどちらか 1 つで、
// provider の中での ID（"<kind>/<name>"）と突き合わせる（E-D2・E-T2）。
type Exclusion struct {
	Owner    string
	ID       string
	IDPrefix string
	Reason   string
}

// Matches は provider の中での ID がこの除外に当たるか。
func (e Exclusion) Matches(id ID) bool {
	if e.ID != "" {
		return id.Local() == e.ID
	}
	return e.IDPrefix != "" && strings.HasPrefix(id.Local(), e.IDPrefix)
}

// FieldType は項目の型（E-B12：順序のある List は全置換、Set は順不同で比べる）。
type FieldType int

const (
	String FieldType = iota
	Int
	Bool
	Duration
	List
	Set
	Map
	// Secret は秘密の参照（{secret: "<backend>:<path>"}）。値を持たず比べず、
	// 世代だけを比べる（E-G2・N-L5）。
	Secret
	// Float は小数の値（/metrics の値・メモリの量）。Condition の大小で比べる
	// （E-H4・N-T4）。
	Float
)

var fieldTypeNames = map[FieldType]string{
	String: "string", Int: "int", Bool: "bool", Duration: "duration",
	List: "list", Set: "set", Map: "map", Secret: "secret", Float: "float",
}

func (t FieldType) String() string { return fieldTypeNames[t] }

// ParseFieldType は型の名前を FieldType にする。
func ParseFieldType(s string) (FieldType, bool) {
	for t, n := range fieldTypeNames {
		if n == s {
			return t, true
		}
	}
	return 0, false
}

// FieldSpec は項目の型と、変われば作り直しになるか（E-A10・N-A8）。
type FieldSpec struct {
	Type     FieldType
	Identity bool
	// Digest は観測が中身の sha256（"sha256:<hex>"）だけを返す項目。比べるときは
	// 宣言の値も Digest にする。中身の平文を観測・plan・Record に出さない
	// （E-E2・E-D5）。Type は String だけ。
	Digest bool
	// Match は宣言の値と観測の値の比べ方を provider が決める項目（provider の
	// 正規形で吸収する、E-I7・E-D1・E-C2）。宣言した slot・キーだけを比べる・
	// 作るときだけ使う（template）・付け替えで名前の変わる volume を比べない、の
	// ように等しさで比べられないもの。observed は観測に項目が無ければ nil。
	// nil なら Type の比べ方（Equal）。
	Match func(desired, observed any) bool
}

// KindSchema は種類ごとの項目。
type KindSchema struct {
	Fields map[string]FieldSpec
	// Settle は Apply のあと読み直すまでの待ち（E-J6・E-R3）。
	Settle      time.Duration
	ObserveOnly bool // 観測だけ。Apply・Delete を呼ばない（E-B7・E-E4・N-B5・N-B6・N-A7）
	// DestructiveDelete は中身を失う種類（volume・DB・バケット）。宣言の外でも
	// prune で消さない（不変条件 9、E-C3・N-C2）。
	DestructiveDelete bool
}

// Schema は provider の種類と項目。Version は比べ方・正規形が変われば上げる
// （E-J12・N-S1・N-P3）。
type Schema struct {
	Version string
	Kinds   map[string]KindSchema
}

// Availability は観測の 4 値（E-K1・N-K1・E-A4）。
type Availability int

const (
	// Unknown は分からない。届かない・読めない・覗けない・定義に合わない出力
	// （E-K1・N-K1・E-A12・E-E6・E-B11）。Absent ではない。ゼロ値を Unknown に
	// して、埋め忘れた観測を「無い」と読まない。
	Unknown Availability = iota
	// Present は在る。
	Present
	// Absent は確かに無い。
	Absent
	// Converging は相手が作業中（E-A4・E-D1・E-B14・E-A1）。
	Converging
)

var availabilityNames = map[Availability]string{
	Unknown: "unknown", Present: "present", Absent: "absent", Converging: "converging",
}

func (a Availability) String() string { return availabilityNames[a] }

// ParseAvailability は名前を Availability にする。
func ParseAvailability(s string) (Availability, bool) {
	for a, n := range availabilityNames {
		if n == s {
			return a, true
		}
	}
	return Unknown, false
}

// MarshalJSON は名前で書く。
func (a Availability) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// UnmarshalJSON は名前を読む。
func (a *Availability) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, ok := ParseAvailability(s)
	if !ok {
		return fmt.Errorf("availability %q を知らない", s)
	}
	*a = v
	return nil
}

// Observation は観測の答え（§3.3）。Fields は Present のときだけ。Note は
// Unknown・Converging の理由。
type Observation struct {
	Availability Availability `json:"availability"`
	Fields       Fields       `json:"fields,omitempty"`
	Note         string       `json:"note,omitempty"`
}

// Listing は List の結果。Complete でなければその範囲を消さない（E-K2・E-F3）。
type Listing struct {
	IDs      []ID
	Complete bool
}

// WriteContext は Apply・Delete のときだけコアが渡す。provider は書き込み用の
// 資格と秘密の値をこの中でだけ取り出す（E-G5・N-G3・E-G2）。
type WriteContext struct {
	_       writeToken
	secrets SecretResolver
}

type writeToken struct{}

// NewWriteContext は秘密の取り出し先を持つ書き込みの文脈を作る。コアの周が
// Apply・Delete・Prepare・Invoke の直前にだけ作る。
func NewWriteContext(s SecretResolver) WriteContext { return WriteContext{secrets: s} }

// Secret は Secret の項目の値（宣言の参照と世代）から秘密の値を取り出す
// （§3.8、E-G2・E-G3・N-G2）。値は使い終えたら捨て、誤りの文・Note・Fields に
// 出さない（不変条件 27）。
func (w WriteContext) Secret(ctx context.Context, value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("秘密の参照でない値")
	}
	ref, err := ParseSecretRef(s)
	if err != nil {
		return "", err
	}
	if w.secrets == nil {
		return "", fmt.Errorf("秘密 %s の取り出し先が無い", ref.Ref())
	}
	return w.secrets.Resolve(ctx, ref)
}

// Observer は観測だけの口。比べる処理（plan）はこれだけを受け取り、現実を
// 変える関数を型として持たない（不変条件 2・30、E-J9）。
type Observer interface {
	Schema() Schema
	// Observe は ID の一覧をまとめて観測する（E-D8・N-D11・N-N1）。返さなかった
	// ID は Unknown として扱う。
	Observe(ctx context.Context, ids []ID) (map[ID]Observation, error)
}

// Lister は範囲の中の ID を列挙する口。prune の根拠は List の ID だけ
// （不変条件 8、E-K2・E-F3）。
type Lister interface {
	List(ctx context.Context, r Range) (Listing, error)
}

// Reader は観測と列挙だけの口。plan と 1 周の判定はこれだけを受け取り、現実を
// 変える関数を型として持たない（不変条件 2・30）。
type Reader interface {
	Observer
	Lister
}

// Provider は 1 つの種類の装置とのやりとり（§3.3）。
type Provider interface {
	Reader
	Apply(ctx context.Context, w WriteContext, id ID, desired Fields, current Observation) error // E-B12・E-D1
	// Delete は断るとき *RefuseError を返す（E-F1・E-G1・E-C2）。
	Delete(ctx context.Context, w WriteContext, id ID, current Observation) error
}

// RefuseError は provider が Delete を断った理由（中身がある・子がある・keep の
// 印がある）。コアは消さずに報告を続け、予算に数えない（不変条件 10、E-F1・E-C2）。
type RefuseError struct {
	Reason string
}

func (e *RefuseError) Error() string { return "削除を断った: " + e.Reason }

// Clock は今の時刻。テストで差し替える（E-R4）。
type Clock interface {
	Now() time.Time
}

// SystemClock は計算機の時計。
type SystemClock struct{}

// Now は time.Now。
func (SystemClock) Now() time.Time { return time.Now() }

// Severity は診断の重さ（§3.8）。
type Severity int

const (
	// Reject は読み込みで断る（E-B5・E-A7・N-B7・E-M3）。
	Reject Severity = iota
	// Warn は知らせるだけ（E-B5）。
	Warn
	// Uncertain は Target の Resource を「分からない」扱いにする（観測も Apply も
	// しない、下流は待つ、E-G4・E-K3）。Target が "<provider>/<種類>" か provider の
	// 名前なら、宣言が欠けているかもしれないので、その provider の範囲を消さない
	// （外部入力が届かないとき空の desired で消さない、E-K3・不変条件 26）。
	Uncertain
)

func (s Severity) String() string {
	switch s {
	case Warn:
		return "warn"
	case Uncertain:
		return "unknown"
	}
	return "reject"
}

// Diagnostic は読み込み・Expand・provider の検証の診断。
type Diagnostic struct {
	Severity Severity
	Origin   Origin
	Target   string // Resource の ID・Range など。無ければ空
	Message  string
}

func (d Diagnostic) String() string {
	var b strings.Builder
	if o := d.Origin.String(); o != "" {
		b.WriteString(o)
		b.WriteString(": ")
	}
	if d.Target != "" {
		b.WriteString(d.Target)
		b.WriteString(": ")
	}
	b.WriteString(d.Message)
	return b.String()
}

// Validator は読み込みのときに provider 固有の誤り（範囲の外の宣言・範囲の
// 重なりなど）を断る任意の口（E-I7・E-A4・E-B3・E-I6・E-K8）。
type Validator interface {
	Validate(res []Resource, ranges []Range) []Diagnostic
}

// Hint は状況で決まる変更の性質（§3.3 の Classifier、E-C2・E-B6・E-C1）。
type Hint struct {
	Field string
	// Destructive は壊す変更（予算の壊す変更に数える、不変条件 9）。
	Destructive bool
	// NeedsApproval は承認が揃うまで当てない変更（E-C2）。当たる Policy に
	// approval が無ければ、申請者でない 1 人の承認を求める。
	NeedsApproval bool
	Reason        string
}

// Classifier は状況で壊す・承認が要る変更（volume の付け替え・規則の変更・
// 保持の設定の変更）を計画のときに言う任意の口（E-C2・E-B6・E-C1）。Schema の
// 静的な印で言えないものだけに使う。現実に触れない（観測 current と宣言から
// 決める）。
type Classifier interface {
	Classify(id ID, current Observation, desired Fields) []Hint
}

// RejectError は読み込みで断った診断の一覧（終了コード 2）。
type RejectError struct {
	Diagnostics []Diagnostic
}

func (e *RejectError) Error() string {
	lines := make([]string, len(e.Diagnostics))
	for i, d := range e.Diagnostics {
		lines[i] = d.String()
	}
	return strings.Join(lines, "\n")
}
