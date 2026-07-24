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

// yaml.go: stdlib-only な最小 YAML サブセットパーサ（S33266e-2）。
//
// # なぜ自前実装か
//
// 本プロジェクトはオフライン + stdlib-only の制約下にあり、gopkg.in/yaml・
// sigs.k8s.io/yaml 等の外部パーサを一切 import できない。そのため人経路（YAML 手編集）
// のために、理想状態木を表現するのに必要十分な *最小サブセット* だけを受理するパーサを
// 自前で実装する。JSON（機械経路）と同じ in-memory 表現（map[string]any / []any /
// スカラー）へパースするので、下流（intentFile へのデコード・JSON Schema 検証・diff）は
// フォーマット非依存になる。
//
// # 受理するサブセット（明示的境界）
//
//   - スカラー: string / int / bool / null。
//   - マップ（ネスト可）: "key: value" 行。キーは bare または引用符付き。
//   - リスト: "- item" 行（ネスト可、マップ要素可）。
//   - snake_case キー（英数と _ / - を許容）。
//   - "#" コメント（行頭・行中とも。ただし引用符内の # はコメントにしない）。
//   - ブロックスタイル（インデント = 半角スペースのみ）。
//   - 先頭に高々 1 個の "---"（単一ドキュメント区切り）。
//   - 空行。
//
// # 黙って誤解釈せず *明確なエラーで拒否* する構文（安全ガード）
//
//   - アンカー / エイリアス（& / *）。
//   - タグ（! で始まるスカラー）。
//   - フロースタイル（{ } / [ ]）。
//   - 複数ドキュメント（2 個目以降の "---"、および "..." 終端）。
//   - マージキー（<<）。
//   - ブロックスカラー（| / >）。
//   - タブによるインデント（インデント曖昧化の温床）。
//   - その他サブセット外の構文。
//
// 部分パーサが安全なのは、この "曖昧なら黙認せず落ちる" ガードがあるからである。
//
// # Norway 問題（型曖昧性）の扱い
//
// bare スカラーの bool 判定は YAML1.1 の広い集合（yes/no/on/off/y/n 等）ではなく、
// **true / false だけ** に限定する。したがって:
//
//   - country: NO      → 文字列 "NO"（false に化けない）。
//   - enabled: on      → 文字列 "on"（true に化けない）。
//   - version: 1.10    → 後述の通り float 化しない。
//
// 数値は **整数のみ** を数値化し、小数点や指数を含むトークン（1.10 / 1e3）は数値化せず
// **文字列のまま** 残す。これにより version: 1.10 が 1.1 の float に潰れる事故を防ぐ。
// 「version は文字列でなければならない」等の意図の最終強制は JSON Schema 層（schema.go）が
// 行う。パーサは *保守的に文字列へ倒す* ことで、スキーマが型を判定できる素材を壊さない。
package loader

import (
	"fmt"
	"strconv"
	"strings"
)

// parseYAML は YAML サブセットのバイト列を JSON と等価な in-memory 表現へパースする。
// トップレベルは map[string]any を返す（理想状態木のルートは常にマップ）。
//
// サブセット外の構文に遭遇した場合は、行番号付きの明確なエラーを返す（黙って誤解釈しない）。
func parseYAML(data []byte) (map[string]any, error) {
	p := &yamlParser{}
	lines, err := p.tokenize(string(data))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	p.lines = lines
	val, err := p.parseBlock(0, 0)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.lines) {
		return nil, fmt.Errorf("yaml: %d 行目: インデントが揃っていません（サブセット外）", p.lines[p.pos].num)
	}
	m, ok := val.(map[string]any)
	if !ok {
		// トップレベルがリストやスカラーの理想状態木は本ローダの想定外。
		return nil, fmt.Errorf("yaml: トップレベルはマップでなければなりません（resources: ... 形式）")
	}
	return m, nil
}

// yamlLine は 1 行分のトークン化結果（インデント除去済み本文 + メタ情報）。
type yamlLine struct {
	num    int    // 元ファイルの 1 始まり行番号
	indent int    // 先頭スペース数
	body   string // インデントとコメントを除いた本文
}

type yamlParser struct {
	lines []yamlLine
	pos   int
}

// tokenize は生テキストを有意な行へ分解する。
// 空行・コメント行は捨てる。タブインデント・アンカー等サブセット外はここで拒否する。
func (p *yamlParser) tokenize(text string) ([]yamlLine, error) {
	rawLines := strings.Split(text, "\n")
	var out []yamlLine
	docSepSeen := false
	for i, raw := range rawLines {
		num := i + 1
		// 行末の \r を除去（CRLF 対応）。
		raw = strings.TrimRight(raw, "\r")

		// タブインデントの拒否（インデント曖昧化の温床）。
		if strings.HasPrefix(raw, "\t") {
			return nil, fmt.Errorf("yaml: %d 行目: タブによるインデントは非対応です（半角スペースを使ってください）", num)
		}

		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		content := raw[indent:]
		if strings.Contains(content, "\t") {
			// 本文中のタブ（キーと値の区切り等）も曖昧なので拒否。
			return nil, fmt.Errorf("yaml: %d 行目: 本文中のタブは非対応です", num)
		}

		trimmed := strings.TrimSpace(content)
		if trimmed == "" {
			continue // 空行
		}
		if strings.HasPrefix(trimmed, "#") {
			continue // コメント行
		}

		// ドキュメント区切り / 終端の扱い。
		if trimmed == "---" {
			if docSepSeen || len(out) > 0 {
				return nil, fmt.Errorf("yaml: %d 行目: 複数ドキュメント（2 個目以降の '---'）は非対応です", num)
			}
			docSepSeen = true
			continue
		}
		if trimmed == "..." {
			return nil, fmt.Errorf("yaml: %d 行目: ドキュメント終端 '...' は非対応です", num)
		}

		// 行頭のサブセット外構文を早期に拒否。
		if err := rejectUnsupported(trimmed, num); err != nil {
			return nil, err
		}

		// コメントを剥がす（引用符外の ' #' のみ）。
		body := stripComment(content)
		body = strings.TrimRight(body, " ")

		out = append(out, yamlLine{num: num, indent: indent, body: body})
	}
	return out, nil
}

// rejectUnsupported は行本文に含まれるサブセット外構文を検出して拒否する。
func rejectUnsupported(trimmed string, num int) error {
	// フロースタイル。
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return fmt.Errorf("yaml: %d 行目: フロースタイル（{}/[]）は非対応です（ブロックスタイルを使ってください）", num)
	}
	// マージキー。
	if strings.HasPrefix(trimmed, "<<") {
		return fmt.Errorf("yaml: %d 行目: マージキー（<<）は非対応です", num)
	}
	return nil
}

// stripComment は引用符の外にある " #"（末尾コメント）を除去する。
// 引用符内の # は保持する。
func stripComment(s string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				// # は直前が行頭または空白のときのみコメント開始。
				if i == 0 || s[i-1] == ' ' {
					return s[:i]
				}
			}
		}
	}
	return s
}

// parseBlock は現在位置から、少なくとも minIndent のインデントを持つ連続ブロックを
// パースする。ブロックがリスト（"- " 始まり）ならスライス、そうでなければマップを返す。
func (p *yamlParser) parseBlock(minIndent, _ int) (any, error) {
	if p.pos >= len(p.lines) {
		return map[string]any{}, nil
	}
	first := p.lines[p.pos]
	if first.indent < minIndent {
		return map[string]any{}, nil
	}
	blockIndent := first.indent
	if strings.HasPrefix(first.body, "- ") || first.body == "-" {
		return p.parseList(blockIndent)
	}
	return p.parseMap(blockIndent)
}

// parseMap は blockIndent のインデントを持つマップエントリ列をパースする。
func (p *yamlParser) parseMap(blockIndent int) (any, error) {
	m := map[string]any{}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < blockIndent {
			break
		}
		if line.indent > blockIndent {
			return nil, fmt.Errorf("yaml: %d 行目: 予期しないインデント（サブセット外）", line.num)
		}
		if strings.HasPrefix(line.body, "- ") || line.body == "-" {
			return nil, fmt.Errorf("yaml: %d 行目: マップの途中にリスト項目が現れました（インデント不整合）", line.num)
		}

		key, rest, err := splitMapEntry(line.body, line.num)
		if err != nil {
			return nil, err
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("yaml: %d 行目: キー %q が重複しています", line.num, key)
		}
		p.pos++

		if rest != "" {
			// インライン値。
			v, err := parseScalar(rest, line.num)
			if err != nil {
				return nil, err
			}
			m[key] = v
			continue
		}
		// 値が次行以降のネストブロック。
		if p.pos < len(p.lines) && p.lines[p.pos].indent > blockIndent {
			child, err := p.parseBlock(blockIndent+1, 0)
			if err != nil {
				return nil, err
			}
			m[key] = child
		} else {
			// "key:" のみ → 空値は null 扱い。
			m[key] = nil
		}
	}
	return m, nil
}

// parseList は blockIndent のインデントを持つ "- " 項目列をパースする。
func (p *yamlParser) parseList(blockIndent int) (any, error) {
	list := []any{}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < blockIndent {
			break
		}
		if line.indent > blockIndent {
			return nil, fmt.Errorf("yaml: %d 行目: 予期しないインデント（サブセット外）", line.num)
		}
		if !(strings.HasPrefix(line.body, "- ") || line.body == "-") {
			break // 別のマップへ戻る
		}

		item := strings.TrimPrefix(line.body, "-")
		item = strings.TrimPrefix(item, " ")
		p.pos++

		if item == "" {
			// "-" のみ → 次行以降のネストブロックが要素。
			if p.pos < len(p.lines) && p.lines[p.pos].indent > blockIndent {
				child, err := p.parseBlock(blockIndent+1, 0)
				if err != nil {
					return nil, err
				}
				list = append(list, child)
			} else {
				list = append(list, nil)
			}
			continue
		}

		// "- key: value" のようにインラインでマップが始まる場合。
		if k, rest, isMap := peekMapEntry(item); isMap {
			// 仮想的に同一行から始まるマップとして扱う。要素インデントは
			// ダッシュ + 空白ぶんずれるが、本サブセットでは "- key: value" の
			// 1 行 or 単純スカラーのみを許可する（複数キーの inline は次行継続で表現）。
			sub := map[string]any{}
			v, err := parseScalar(rest, line.num)
			if err != nil {
				return nil, err
			}
			sub[k] = v
			// 続く行が blockIndent より深ければ、同じマップの続きとして取り込む。
			contIndent := blockIndent + 2 // "- " のぶん
			for p.pos < len(p.lines) && p.lines[p.pos].indent >= contIndent {
				cont := p.lines[p.pos]
				if strings.HasPrefix(cont.body, "- ") || cont.body == "-" {
					break
				}
				ck, crest, err := splitMapEntry(cont.body, cont.num)
				if err != nil {
					return nil, err
				}
				if _, dup := sub[ck]; dup {
					return nil, fmt.Errorf("yaml: %d 行目: キー %q が重複しています", cont.num, ck)
				}
				p.pos++
				if crest != "" {
					cv, err := parseScalar(crest, cont.num)
					if err != nil {
						return nil, err
					}
					sub[ck] = cv
				} else if p.pos < len(p.lines) && p.lines[p.pos].indent > cont.indent {
					child, err := p.parseBlock(cont.indent+1, 0)
					if err != nil {
						return nil, err
					}
					sub[ck] = child
				} else {
					sub[ck] = nil
				}
			}
			list = append(list, sub)
			continue
		}

		// 単純スカラー要素。
		v, err := parseScalar(item, line.num)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	return list, nil
}

// splitMapEntry は "key: value" を key と value(rest) に分ける。
// value が空（"key:"）のとき rest == ""。
func splitMapEntry(body string, num int) (key, rest string, err error) {
	k, r, ok := peekMapEntryRaw(body)
	if !ok {
		return "", "", fmt.Errorf("yaml: %d 行目: マップエントリ 'key: value' の形式ではありません: %q", num, body)
	}
	return k, r, nil
}

// peekMapEntry は body がマップエントリなら (key, rest, true) を返す。
func peekMapEntry(body string) (key, rest string, ok bool) {
	return peekMapEntryRaw(body)
}

// peekMapEntryRaw は引用符を考慮して最初の ": "（または末尾 ":"）でキーと値を分割する。
func peekMapEntryRaw(body string) (key, rest string, ok bool) {
	inSingle, inDouble := false, false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ':':
			if inSingle || inDouble {
				continue
			}
			// ": " または 行末の ":" がキー区切り。
			if i+1 >= len(body) {
				key = unquoteKey(body[:i])
				return key, "", key != ""
			}
			if body[i+1] == ' ' {
				key = unquoteKey(body[:i])
				rest = strings.TrimSpace(body[i+1:])
				return key, rest, key != ""
			}
			// "key:value"（空白なし）はコロンを含む bare スカラーの可能性があるので
			// キー区切りとしては採用しない。
		}
	}
	return "", "", false
}

// unquoteKey はキー側の引用符を外し、前後空白を除く。
func unquoteKey(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// parseScalar はスカラー文字列トークンを Go の値へ変換する。
//
// 型判定の規則（Norway 問題対策）:
//   - 引用符付き → 常に string（型ヒント: 明示引用符は文字列意図）。
//   - null / ~ / 空 → nil。
//   - true / false（小文字のみ） → bool。yes/no/on/off 等は *文字列のまま*。
//   - 整数トークン（[-+]?digits）→ int。
//   - 小数点・指数を含むトークン（1.10 / 1e3）→ 数値化せず string のまま。
//   - それ以外 → string。
func parseScalar(tok string, num int) (any, error) {
	tok = strings.TrimSpace(tok)

	// 引用符付き文字列。
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		return unquoteDouble(tok, num)
	}
	if len(tok) >= 2 && tok[0] == '\'' && tok[len(tok)-1] == '\'' {
		// 単一引用符: エスケープなし（'' → ' のみ）。
		inner := tok[1 : len(tok)-1]
		return strings.ReplaceAll(inner, "''", "'"), nil
	}

	// サブセット外構文の拒否（bare スカラー位置）。
	if err := rejectScalarUnsupported(tok, num); err != nil {
		return nil, err
	}

	// null 系。
	if tok == "" || tok == "null" || tok == "~" || tok == "Null" || tok == "NULL" {
		return nil, nil
	}
	// bool は true/false のみ（Norway 問題対策で yes/no/on/off を含めない）。
	if tok == "true" {
		return true, nil
	}
	if tok == "false" {
		return false, nil
	}
	// 整数のみ数値化（小数・指数は文字列のまま残す）。
	if isIntToken(tok) {
		if n, err := strconv.Atoi(tok); err == nil {
			return n, nil
		}
	}
	// それ以外は bare 文字列（version: 1.10 / country: NO などはここで文字列に落ちる）。
	return tok, nil
}

// rejectScalarUnsupported は bare スカラー位置のサブセット外構文を拒否する。
func rejectScalarUnsupported(tok string, num int) error {
	switch tok[0] {
	case '&':
		return fmt.Errorf("yaml: %d 行目: アンカー（&）は非対応です", num)
	case '*':
		return fmt.Errorf("yaml: %d 行目: エイリアス（*）は非対応です", num)
	case '!':
		return fmt.Errorf("yaml: %d 行目: タグ（!）は非対応です", num)
	case '|', '>':
		return fmt.Errorf("yaml: %d 行目: ブロックスカラー（|/>）は非対応です", num)
	case '{', '[':
		return fmt.Errorf("yaml: %d 行目: フロースタイル（{}/[]）は非対応です", num)
	}
	return nil
}

// isIntToken は tok が整数リテラルか判定する。小数点・指数・16進等は false。
//
// S33266e-2: 先頭ゼロを持つ多桁トークン（007 / 010 / -042 など）は int にしない。
// ゼロ詰めのコード/識別子（例: code: 007）を int 化すると "7" へ潰れてデータが失われ、
// 010 は 8進のように誤解される。float の "1.10" を文字列に残す保守姿勢と揃え、
// 先頭ゼロ+後続数字は文字列のまま残す。単独の "0" は int 0 として扱う。
func isIntToken(tok string) bool {
	if tok == "" {
		return false
	}
	i := 0
	if tok[0] == '+' || tok[0] == '-' {
		i = 1
	}
	if i >= len(tok) {
		return false
	}
	// 先頭ゼロ+後続数字は非 int（文字列のまま残しデータ損失を防ぐ）。単独の 0 は許可。
	if tok[i] == '0' && i+1 < len(tok) {
		return false
	}
	for ; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

// unquoteDouble は二重引用符文字列を最小限のエスケープ処理付きで復号する。
func unquoteDouble(tok string, num int) (string, error) {
	inner := tok[1 : len(tok)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(inner) {
			return "", fmt.Errorf("yaml: %d 行目: 引用符文字列末尾の不正なエスケープ", num)
		}
		switch inner[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			return "", fmt.Errorf("yaml: %d 行目: 未対応のエスケープ \\%c", num, inner[i])
		}
	}
	return b.String(), nil
}
