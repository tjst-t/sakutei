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

// loader_test.go: Story S33266e-2 の受入テスト。
//
// カバー範囲:
//   - AC-S33266e-2-1: JSON と YAML の同一意味入力が同一 DesiredResource 列を生む。
//   - AC-S33266e-2-2: round-trip 安定性（load→emit→load が等価）。
//   - AC-S33266e-2-3: JSON Schema 検証がロード時に強制される（不正 intent を拒否）。
//   - AC-S33266e-2-4: YAML サブセット外構文（アンカー/タブ/複数ドキュメント）を拒否。
//   - Norway 問題: version:1.10 が文字列のまま／country:NO が false に化けない。
package acceptance

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/loader"
)

// writeIntent は tmp ディレクトリに 1 ファイルを書き、そのディレクトリパスを返す。
func writeIntentDir(t *testing.T, filename, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeFileAtomic(filepath.Join(dir, filename), []byte(content), 0o600); err != nil {
		t.Fatalf("write intent file: %v", err)
	}
	return dir
}

// resourcesByLocalID は比較を安定させるため Local ID → DesiredResource のマップを作る。
func resourcesByLocalID(rs []diff.DesiredResource) map[string]diff.DesiredResource {
	m := make(map[string]diff.DesiredResource, len(rs))
	for _, r := range rs {
		m[r.ID.Local] = r
	}
	return m
}

// ---------------------------------------------------------------------------
// AC-S33266e-2-1: JSON と YAML の同一意味入力が同一 DesiredResource 列を生む。
// ---------------------------------------------------------------------------

func TestLoader_JSONAndYAMLEqualResources(t *testing.T) {
	// [AC-S33266e-2-1]
	jsonSrc := `{
  "resources": [
    {
      "provider": "fake",
      "id": "web-1",
      "scope": {"site": "tokyo"},
      "depends_on": ["db-1"],
      "fields": {"name": "web-1", "size": "10"},
      "deletion_policy": "PruneWithinScope"
    },
    {
      "provider": "fake",
      "id": "db-1",
      "fields": {"name": "db-1"}
    }
  ]
}`

	yamlSrc := `# 人経路（YAML 手編集）— 上の JSON と同一意味
resources:
  - provider: fake
    id: web-1
    scope:
      site: tokyo
    depends_on:
      - db-1
    fields:
      name: web-1
      size: "10"
    deletion_policy: PruneWithinScope
  - provider: fake
    id: db-1
    fields:
      name: db-1
`

	jsonDir := writeIntentDir(t, "intent.json", jsonSrc)
	yamlDir := writeIntentDir(t, "intent.yaml", yamlSrc)

	jsonRes, _, err := loader.LoadDir(jsonDir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-1] LoadDir(json): %v", err)
	}
	yamlRes, _, err := loader.LoadDir(yamlDir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-1] LoadDir(yaml): %v", err)
	}

	jm := resourcesByLocalID(jsonRes)
	ym := resourcesByLocalID(yamlRes)
	if !reflect.DeepEqual(jm, ym) {
		t.Fatalf("[AC-S33266e-2-1] JSON と YAML が同一意味なのに DesiredResource が不一致:\n json=%#v\n yaml=%#v", jm, ym)
	}
}

// AC-S33266e-2-1（続き）: 同一ディレクトリに .json/.yaml/.yml を混在させても
// フォーマットに依らず読み込める。
func TestLoader_MixedFormatsInSameDir(t *testing.T) {
	// [AC-S33266e-2-1]
	dir := t.TempDir()
	files := map[string]string{
		"a.json": `{"resources":[{"provider":"fake","id":"a","fields":{"name":"a"}}]}`,
		"b.yaml": "resources:\n  - provider: fake\n    id: b\n    fields:\n      name: b\n",
		"c.yml":  "resources:\n  - provider: fake\n    id: c\n    fields:\n      name: c\n",
	}
	for name, content := range files {
		if err := writeFileAtomic(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	res, _, err := loader.LoadDir(dir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-1] LoadDir(mixed): %v", err)
	}
	got := resourcesByLocalID(res)
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := got[id]; !ok {
			t.Errorf("[AC-S33266e-2-1] 混在ディレクトリで id %q が読み込まれていない", id)
		}
	}
}

// ---------------------------------------------------------------------------
// AC-S33266e-2-2: round-trip 安定性（load→emit→load が等価な木を返す）。
// ---------------------------------------------------------------------------

func TestLoader_RoundTripStable(t *testing.T) {
	// [AC-S33266e-2-2]
	yamlSrc := `resources:
  - provider: fake
    id: web-1
    scope:
      site: tokyo
    fields:
      name: web-1
      size: "10"
      version: "1.10"
    deletion_policy: ExplicitAbsentOnly
`
	// load(YAML) → 木1
	tree1, err := loader.DecodeIntentTree([]byte(yamlSrc), "yaml")
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] decode yaml: %v", err)
	}
	if err := loader.ValidateIntentTree(tree1); err != nil {
		t.Fatalf("[AC-S33266e-2-2] validate tree1: %v", err)
	}

	// emit(木1) → canonical JSON
	emitted, err := loader.EmitCanonicalJSON(tree1)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] emit: %v", err)
	}

	// load(JSON) → 木2
	tree2, err := loader.DecodeIntentTree(emitted, "json")
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] decode emitted json: %v", err)
	}

	if !reflect.DeepEqual(tree1, tree2) {
		t.Fatalf("[AC-S33266e-2-2] round-trip 不安定:\n tree1=%#v\n tree2=%#v", tree1, tree2)
	}

	// emit をもう一度通しても決定的（バイト一致）であること。
	emitted2, err := loader.EmitCanonicalJSON(tree2)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] emit2: %v", err)
	}
	if string(emitted) != string(emitted2) {
		t.Fatalf("[AC-S33266e-2-2] emit が非決定的:\n a=%s\n b=%s", emitted, emitted2)
	}
}

// round-trip をモデルレベル（DesiredResource）でも確認する: YAML から読んだ列と、
// それを emit した JSON から読んだ列が等価。
func TestLoader_RoundTripModelLevel(t *testing.T) {
	// [AC-S33266e-2-2]
	yamlSrc := `resources:
  - provider: fake
    id: db-1
    fields:
      name: db-1
      size: "5"
`
	yamlDir := writeIntentDir(t, "intent.yaml", yamlSrc)
	yamlRes, _, err := loader.LoadDir(yamlDir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] LoadDir(yaml): %v", err)
	}

	tree, err := loader.DecodeIntentTree([]byte(yamlSrc), "yaml")
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] decode: %v", err)
	}
	emitted, err := loader.EmitCanonicalJSON(tree)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] emit: %v", err)
	}
	jsonDir := writeIntentDir(t, "intent.json", string(emitted))
	jsonRes, _, err := loader.LoadDir(jsonDir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-2] LoadDir(emitted json): %v", err)
	}

	if !reflect.DeepEqual(resourcesByLocalID(yamlRes), resourcesByLocalID(jsonRes)) {
		t.Fatalf("[AC-S33266e-2-2] モデルレベル round-trip 不一致")
	}
}

// ---------------------------------------------------------------------------
// AC-S33266e-2-3: JSON Schema 検証がロード時に強制される。
// ---------------------------------------------------------------------------

func TestLoader_SchemaRejectsMissingRequired(t *testing.T) {
	// [AC-S33266e-2-3] provider が必須なのに欠落 → 拒否。
	src := `{"resources":[{"id":"x","fields":{"name":"x"}}]}`
	dir := writeIntentDir(t, "bad.json", src)
	_, _, err := loader.LoadDir(dir)
	if err == nil {
		t.Fatalf("[AC-S33266e-2-3] provider 欠落の intent が受理された（拒否されるべき）")
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("[AC-S33266e-2-3] エラーが違反フィールド provider を示していない: %v", err)
	}
}

func TestLoader_SchemaRejectsUnknownKey(t *testing.T) {
	// [AC-S33266e-2-3] 未知キー（typo など）を additionalProperties:false で拒否。
	src := `{"resources":[{"provider":"fake","id":"x","feilds":{"name":"x"}}]}`
	dir := writeIntentDir(t, "bad.json", src)
	_, _, err := loader.LoadDir(dir)
	if err == nil {
		t.Fatalf("[AC-S33266e-2-3] 未知キー feilds を含む intent が受理された")
	}
	if !strings.Contains(err.Error(), "feilds") {
		t.Errorf("[AC-S33266e-2-3] エラーが未知キー feilds を示していない: %v", err)
	}
}

func TestLoader_SchemaRejectsBadEnum(t *testing.T) {
	// [AC-S33266e-2-3] deletion_policy の enum 逸脱を拒否。
	src := `{"resources":[{"provider":"fake","id":"x","fields":{"name":"x"},"deletion_policy":"NukeEverything"}]}`
	dir := writeIntentDir(t, "bad.json", src)
	_, _, err := loader.LoadDir(dir)
	if err == nil {
		t.Fatalf("[AC-S33266e-2-3] 不正な deletion_policy が受理された")
	}
	if !strings.Contains(err.Error(), "deletion_policy") {
		t.Errorf("[AC-S33266e-2-3] エラーが deletion_policy を示していない: %v", err)
	}
}

func TestLoader_SchemaAcceptsValid(t *testing.T) {
	// [AC-S33266e-2-3] 妥当な intent は受理される。
	src := `{"resources":[{"provider":"fake","id":"x","fields":{"name":"x"},"deletion_policy":"PruneWithinScope"}]}`
	dir := writeIntentDir(t, "ok.json", src)
	res, _, err := loader.LoadDir(dir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-3] 妥当な intent が拒否された: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("[AC-S33266e-2-3] resource 数が想定外: %d", len(res))
	}
}

// ---------------------------------------------------------------------------
// AC-S33266e-2-4: YAML サブセット外構文を明確なエラーで拒否する。
// ---------------------------------------------------------------------------

func TestLoader_YAMLRejectsAnchor(t *testing.T) {
	// [AC-S33266e-2-4] アンカー/エイリアスは拒否。
	src := "resources:\n  - provider: fake\n    id: x\n    fields: &anchor\n      name: x\n"
	dir := writeIntentDir(t, "anchor.yaml", src)
	_, _, err := loader.LoadDir(dir)
	if err == nil {
		t.Fatalf("[AC-S33266e-2-4] アンカーを含む YAML が受理された（拒否されるべき）")
	}
	if !strings.Contains(err.Error(), "アンカー") {
		t.Errorf("[AC-S33266e-2-4] アンカー拒否のエラーメッセージが不明瞭: %v", err)
	}
}

func TestLoader_YAMLRejectsTabIndent(t *testing.T) {
	// [AC-S33266e-2-4] タブインデントは拒否。
	src := "resources:\n\t- provider: fake\n"
	dir := writeIntentDir(t, "tab.yaml", src)
	_, _, err := loader.LoadDir(dir)
	if err == nil {
		t.Fatalf("[AC-S33266e-2-4] タブインデントの YAML が受理された")
	}
	if !strings.Contains(err.Error(), "タブ") {
		t.Errorf("[AC-S33266e-2-4] タブ拒否のエラーメッセージが不明瞭: %v", err)
	}
}

func TestLoader_YAMLRejectsMultiDocument(t *testing.T) {
	// [AC-S33266e-2-4] 複数ドキュメント（2 個目以降の '---'）は拒否。
	src := "resources:\n  - provider: fake\n    id: x\n    fields:\n      name: x\n---\nresources: []\n"
	dir := writeIntentDir(t, "multi.yaml", src)
	_, _, err := loader.LoadDir(dir)
	if err == nil {
		t.Fatalf("[AC-S33266e-2-4] 複数ドキュメントの YAML が受理された")
	}
	if !strings.Contains(err.Error(), "複数ドキュメント") {
		t.Errorf("[AC-S33266e-2-4] 複数ドキュメント拒否のエラーが不明瞭: %v", err)
	}
}

func TestLoader_YAMLRejectsFlowStyle(t *testing.T) {
	// [AC-S33266e-2-4] フロースタイルは拒否。
	src := "resources: [ {provider: fake, id: x} ]\n"
	dir := writeIntentDir(t, "flow.yaml", src)
	_, _, err := loader.LoadDir(dir)
	if err == nil {
		t.Fatalf("[AC-S33266e-2-4] フロースタイルの YAML が受理された")
	}
	if !strings.Contains(err.Error(), "フロースタイル") {
		t.Errorf("[AC-S33266e-2-4] フロースタイル拒否のエラーが不明瞭: %v", err)
	}
}

// 先頭 1 個の '---'（単一ドキュメント区切り）は許容される。
func TestLoader_YAMLAllowsLeadingDocSeparator(t *testing.T) {
	// [AC-S33266e-2-4]
	src := "---\nresources:\n  - provider: fake\n    id: x\n    fields:\n      name: x\n"
	dir := writeIntentDir(t, "lead.yaml", src)
	res, _, err := loader.LoadDir(dir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-4] 先頭 '---' 付き単一ドキュメントが拒否された: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("[AC-S33266e-2-4] resource 数が想定外: %d", len(res))
	}
}

// ---------------------------------------------------------------------------
// Norway 問題: 型曖昧性を保守的に扱い、スキーマで最終強制する。
// ---------------------------------------------------------------------------

func TestLoader_NorwayProblem_VersionStaysString(t *testing.T) {
	// [AC-S33266e-2-3] version: 1.10 は引用符なしでも float 化せず文字列のまま。
	// トップレベル version はスキーマで string 必須なので、パーサが文字列に倒す限り受理される。
	src := "version: 1.10\nresources:\n  - provider: fake\n    id: x\n    fields:\n      name: x\n"
	tree, err := loader.DecodeIntentTree([]byte(src), "yaml")
	if err != nil {
		t.Fatalf("[AC-S33266e-2-3] decode: %v", err)
	}
	v, ok := tree["version"].(string)
	if !ok {
		t.Fatalf("[AC-S33266e-2-3] version が文字列でない（Norway 問題: float 化した）: %T %v", tree["version"], tree["version"])
	}
	if v != "1.10" {
		t.Errorf("[AC-S33266e-2-3] version の値が変質した: %q（期待 \"1.10\"）", v)
	}
	// スキーマ検証も通ること（version は string 必須）。
	if err := loader.ValidateIntentTree(tree); err != nil {
		t.Fatalf("[AC-S33266e-2-3] 妥当な version:1.10 がスキーマで拒否された: %v", err)
	}
}

func TestLoader_NorwayProblem_CountryNotCoercedToBool(t *testing.T) {
	// [AC-S33266e-2-3] country: NO（引用符なし）が false に化けない。
	src := "resources:\n  - provider: fake\n    id: x\n    fields:\n      country: NO\n      enabled: on\n"
	tree, err := loader.DecodeIntentTree([]byte(src), "yaml")
	if err != nil {
		t.Fatalf("[AC-S33266e-2-3] decode: %v", err)
	}
	res := tree["resources"].([]any)[0].(map[string]any)
	fields := res["fields"].(map[string]any)
	if got := fields["country"]; got != "NO" {
		t.Errorf("[AC-S33266e-2-3] country が bool に化けた: %T %v（期待 文字列 \"NO\"）", got, got)
	}
	if got := fields["enabled"]; got != "on" {
		t.Errorf("[AC-S33266e-2-3] enabled:on が bool に化けた: %T %v（期待 文字列 \"on\"）", got, got)
	}
}

func TestLoader_ScalarTypes(t *testing.T) {
	// [AC-S33266e-2-3] true/false のみ bool、整数のみ int、それ以外は string。
	src := "version: \"1.0\"\nresources:\n  - provider: fake\n    id: x\n    fields:\n      flag: true\n      count: 42\n      label: hello\n"
	tree, err := loader.DecodeIntentTree([]byte(src), "yaml")
	if err != nil {
		t.Fatalf("[AC-S33266e-2-3] decode: %v", err)
	}
	fields := tree["resources"].([]any)[0].(map[string]any)["fields"].(map[string]any)
	if fields["flag"] != true {
		t.Errorf("flag は bool true であるべき: %T %v", fields["flag"], fields["flag"])
	}
	if fields["count"] != 42 {
		t.Errorf("count は int 42 であるべき: %T %v", fields["count"], fields["count"])
	}
	if fields["label"] != "hello" {
		t.Errorf("label は string であるべき: %T %v", fields["label"], fields["label"])
	}
}

// ---------------------------------------------------------------------------
// AC-S33266e-2-5: 先頭ゼロの数値トークンは文字列のまま残す（データ損失防止）。
//
// code: 007 を int 7 → "7" に潰すと、ゼロ詰めコード/識別子が黙って壊れる。float の
// 1.10 を文字列に残す保守姿勢と揃え、先頭ゼロ+後続数字は string、単独の 0 は int 0。
// ---------------------------------------------------------------------------

func TestLoader_LeadingZeroStaysString(t *testing.T) {
	// [AC-S33266e-2-5]
	src := "resources:\n" +
		"  - provider: fake\n" +
		"    id: x\n" +
		"    fields:\n" +
		"      code: 007\n" +
		"      octalish: 010\n" +
		"      zero: 0\n" +
		"      plain: 42\n" +
		"      neg: -042\n"
	tree, err := loader.DecodeIntentTree([]byte(src), "yaml")
	if err != nil {
		t.Fatalf("[AC-S33266e-2-5] decode: %v", err)
	}
	fields := tree["resources"].([]any)[0].(map[string]any)["fields"].(map[string]any)

	if got := fields["code"]; got != "007" {
		t.Errorf("[AC-S33266e-2-5] code: 007 は string \"007\" であるべき: %T %v", got, got)
	}
	if got := fields["octalish"]; got != "010" {
		t.Errorf("[AC-S33266e-2-5] octalish: 010 は string \"010\" であるべき: %T %v", got, got)
	}
	if got := fields["zero"]; got != 0 {
		t.Errorf("[AC-S33266e-2-5] zero: 0 は int 0 であるべき: %T %v", got, got)
	}
	if got := fields["plain"]; got != 42 {
		t.Errorf("[AC-S33266e-2-5] plain: 42 は int 42 であるべき: %T %v", got, got)
	}
	if got := fields["neg"]; got != "-042" {
		t.Errorf("[AC-S33266e-2-5] neg: -042 は string \"-042\" であるべき: %T %v", got, got)
	}
}

// AC-S33266e-2-5（end-to-end）: LoadDir を通しても 007 が "007" のまま field 値に残る。
func TestLoader_LeadingZeroEndToEnd(t *testing.T) {
	// [AC-S33266e-2-5]
	src := "resources:\n" +
		"  - provider: fake\n" +
		"    id: dev-007\n" +
		"    fields:\n" +
		"      code: 007\n" +
		"      octalish: 010\n" +
		"      zero: 0\n" +
		"      plain: 42\n"
	dir := writeIntentDir(t, "intent.yaml", src)
	res, _, err := loader.LoadDir(dir)
	if err != nil {
		t.Fatalf("[AC-S33266e-2-5] LoadDir: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("[AC-S33266e-2-5] want 1 resource, got %d", len(res))
	}
	f := res[0].Fields
	if f["code"] != "007" {
		t.Errorf("[AC-S33266e-2-5] LoadDir: code は \"007\" であるべき: %q", f["code"])
	}
	if f["octalish"] != "010" {
		t.Errorf("[AC-S33266e-2-5] LoadDir: octalish は \"010\" であるべき: %q", f["octalish"])
	}
	if f["zero"] != "0" {
		t.Errorf("[AC-S33266e-2-5] LoadDir: zero は \"0\" であるべき: %q", f["zero"])
	}
	if f["plain"] != "42" {
		t.Errorf("[AC-S33266e-2-5] LoadDir: plain は \"42\" であるべき: %q", f["plain"])
	}
}
