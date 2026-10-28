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

// nftables_boundary_test.go は Backlog #68 の受け入れテスト: nftables の Apply
// が管理境界の外（base chain の宣言と中身・set/map 等）を壊さないこと。
//
// タグ:
//
//	#68-1  base chain（type・hook・priority・policy 付き）を含む ruleset で
//	       regular chain を reconcile しても、base chain の行がバイト単位で
//	       変わらない。その後の status に base chain が unmanaged として出ない。
//	#68-2  管理サブセットの外の行（table 直下の set/map、table の外の行）は
//	       Apply（Update/Create/Delete）で消えない。書き直せない chain（複数行
//	       の匿名 set を含む等）への Apply は PermanentError で止まり、ファイルは
//	       変わらない。
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// nftBaseChainBlock は再現の ruleset の base chain ブロック（4 スペースの
// インデント。provider が書く形と違うので、書き直されれば分かる）。
const nftBaseChainBlock = `    chain input {
        type filter hook input priority 0; policy drop;
        jump sakutei_allow
    }
`

// nftUpdateOp は chain の rules を rules に変える Update の plan を返す。
func nftUpdateOp(localID, from, to string) core.Plan {
	return core.Plan{Ops: []core.Op{{
		Kind:   core.OpKindUpdate,
		Target: core.NewID(nftables.ProviderName, core.NewScope(nil), localID),
		Fields: []core.FieldDiff{{Field: "rules", From: from, To: to}},
	}}}
}

// readNFTFile は ruleset ファイルの中身を返す。
func readNFTFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ruleset: %v", err)
	}
	return string(b)
}

// TestNFTables_ApplyKeepsBaseChainVerbatim は 2026-10-21 の再現手順をそのまま
// CLI で回す: regular chain の sakutei_allow を reconcile しても、base chain の
// input の宣言行と中身はバイト単位で残る。
//
// [#68-1]
func TestNFTables_ApplyKeepsBaseChainVerbatim(t *testing.T) {
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "firewall.rules")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	before := "table inet filter {\n" + nftBaseChainBlock + `    chain sakutei_allow {
        tcp dport 22 accept
    }
}
`
	writeRulesetFile(t, rulesetPath, before)
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{{
			"provider": "nftables",
			"id":       "inet/filter/sakutei_allow",
			"fields": map[string]string{
				"family": "inet",
				"table":  "filter",
				"chain":  "sakutei_allow",
				"rules":  nftEncoded("tcp dport 22 accept", "tcp dport 443 accept"),
			},
		}},
	})

	stdout, stderr, code := runReconcile(t, intentDir, rulesetPath)
	if code != 0 {
		t.Fatalf("[#68-1] reconcile exited %d; stderr=%s stdout=%s", code, stderr, stdout)
	}

	// 期待: 変わるのは sakutei_allow の中身（443 の行が足される）だけ。base chain
	// の行もインデントも元のまま。
	want := "table inet filter {\n" + nftBaseChainBlock + `    chain sakutei_allow {
        tcp dport 22 accept
        tcp dport 443 accept
    }
}
`
	if got := readNFTFile(t, rulesetPath); got != want {
		t.Fatalf("[#68-1] ruleset after reconcile differs from expected (only sakutei_allow may change)\ngot:\n%s\nwant:\n%s", got, want)
	}

	// base chain は管理の外のまま: status に unmanaged として出ない。
	sout, serr, scode := runStatusCmd(t, intentDir, rulesetPath)
	if scode != 0 {
		t.Fatalf("[#68-1] status exited %d; stderr=%s stdout=%s", scode, serr, sout)
	}
	if strings.Contains(sout, "inet/filter/input") {
		t.Errorf("[#68-1] base chain input must not appear in status (management boundary); got:\n%s", sout)
	}

	// 2 回目は収束済み（書き直しが冪等）。
	stdout2, stderr2, code2 := runReconcile(t, intentDir, rulesetPath)
	if code2 != 0 || !strings.Contains(stdout2, "Converged (no changes)") {
		t.Errorf("[#68-1] 2nd reconcile: exit=%d stdout=%s stderr=%s", code2, stdout2, stderr2)
	}
	t.Logf("[#68-1] PASS: base chain bytes unchanged; only the managed regular chain was rewritten")
}

// TestNFTables_ApplyKeepsLinesOutsideManagedSubset は管理サブセットの外の行が
// Update・Create・Delete のどれでも消えないことを示す。
//
// [#68-2]
func TestNFTables_ApplyKeepsLinesOutsideManagedSubset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outside.rules")
	const header = `#!/usr/sbin/nft -f
flush ruleset
define ssh_port = 22
`
	const tableHead = `table inet filter {
	set allowed {
		type ipv4_addr
		elements = { 192.0.2.1, 192.0.2.2 }
	}
	map portmap { type inet_service : verdict; }
	counter hits {
	}
`
	const baseChain = `	chain input {
		type filter hook input priority 0;
		policy drop;
		ip saddr @allowed accept
	}
`
	const nat = `table ip nat {
	chain postrouting {
		type nat hook postrouting priority 100; policy accept;
		masquerade
	}
}
`
	original := header + tableHead + baseChain + `	chain custom {
		tcp dport 22 accept
	}
}
` + nat
	writeRulesetFile(t, path, original)
	p := nftables.New(path)
	ctx := context.Background()

	// Update: custom の中身だけが変わる。
	if err := p.Apply(ctx, nftUpdateOp("inet/filter/custom", "tcp dport 22 accept",
		nftEncoded("tcp dport 22 accept", "tcp dport 80 accept"))); err != nil {
		t.Fatalf("[#68-2] Update: %v", err)
	}
	afterUpdate := header + tableHead + baseChain + `	chain custom {
		tcp dport 22 accept
		tcp dport 80 accept
	}
}
` + nat
	if got := readNFTFile(t, path); got != afterUpdate {
		t.Fatalf("[#68-2] Update rewrote lines outside the managed chain\ngot:\n%s\nwant:\n%s", got, afterUpdate)
	}

	// Create: 同じ table の閉じ括弧の直前に足され、他は変わらない。
	create := core.Plan{Ops: []core.Op{{
		Kind:   core.OpKindCreate,
		Target: core.NewID(nftables.ProviderName, core.NewScope(nil), "inet/filter/web"),
		Fields: []core.FieldDiff{{Field: "rules", To: "tcp dport 443 accept"}},
	}}}
	if err := p.Apply(ctx, create); err != nil {
		t.Fatalf("[#68-2] Create: %v", err)
	}
	afterCreate := header + tableHead + baseChain + `	chain custom {
		tcp dport 22 accept
		tcp dport 80 accept
	}
	chain web {
		tcp dport 443 accept
	}
}
` + nat
	if got := readNFTFile(t, path); got != afterCreate {
		t.Fatalf("[#68-2] Create rewrote lines outside the new chain\ngot:\n%s\nwant:\n%s", got, afterCreate)
	}

	// Delete（Pruneable:false なので通常は来ないが、明示 opt-in 時の経路）:
	// custom と web を消すと、元のファイルから custom を除いたものに戻る。
	for _, id := range []string{"inet/filter/custom", "inet/filter/web"} {
		del := core.Plan{Ops: []core.Op{{
			Kind:   core.OpKindDelete,
			Target: core.NewID(nftables.ProviderName, core.NewScope(nil), id),
		}}}
		if err := p.Apply(ctx, del); err != nil {
			t.Fatalf("[#68-2] Delete %s: %v", id, err)
		}
	}
	afterDelete := header + tableHead + baseChain + "}\n" + nat
	if got := readNFTFile(t, path); got != afterDelete {
		t.Fatalf("[#68-2] Delete rewrote lines outside the deleted chains\ngot:\n%s\nwant:\n%s", got, afterDelete)
	}

	// base chain（type と policy を別の行に書いたもの）は宣言したときだけ扱う
	// （#173）: Read は type・policy を別の行から読み、宣言が regular chain なら
	// 計画の時点で断る（上の Update・Create・Delete でも行はそのまま）。
	obs, err := p.Read(ctx, "inet/filter/input")
	if err != nil || obs.State["hook"].Value != "input" || obs.State["policy"].Value != "drop" {
		t.Errorf("[#173] base chain with split type/policy lines: Read = %v, %v", obs.State, err)
	}
	if _, err := p.RefinePlan("inet/filter/input", obs.State, map[string]string{"rules": "drop"}, nil); err == nil {
		t.Errorf("[#173] a regular declaration of the base chain must be refused at plan time")
	}
	t.Logf("[#68-2] PASS: set/map/counter/define/other tables and base chains survive Update/Create/Delete")
}

// TestNFTables_ApplyRefusesChainOutsideManagedSubset は、書き直すと中身を
// 失う chain（複数行の匿名 set を含む）への Apply が PermanentError で止まり、
// ファイルが変わらないことを示す。
//
// [#68-2]
func TestNFTables_ApplyRefusesChainOutsideManagedSubset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.rules")
	original := `table ip filter {
	chain custom {
		ip saddr {
			192.0.2.1,
			192.0.2.2
		} accept
	}
	chain oneline { counter drop; }
}
`
	writeRulesetFile(t, path, original)
	p := nftables.New(path)
	ctx := context.Background()

	for _, id := range []string{"ip/filter/custom", "ip/filter/oneline"} {
		err := p.Apply(ctx, nftUpdateOp(id, "", "tcp dport 22 accept"))
		if !sdk.IsPermanent(err) {
			t.Errorf("[#68-2] Apply to %s (syntax outside the managed subset) must be a PermanentError; got %v", id, err)
		}
		del := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: core.NewID(nftables.ProviderName, core.NewScope(nil), id)}}}
		if err := p.Apply(ctx, del); !sdk.IsPermanent(err) {
			t.Errorf("[#68-2] Delete of %s must be a PermanentError; got %v", id, err)
		}
		if _, err := p.Read(ctx, id); !sdk.IsPermanent(err) {
			t.Errorf("[#68-2] Read of %s must be a PermanentError (not silently partial); got %v", id, err)
		}
	}
	if got := readNFTFile(t, path); got != original {
		t.Fatalf("[#68-2] refused Apply must not touch the file\ngot:\n%s\nwant:\n%s", got, original)
	}

	// List にも出さない（管理対象外）。
	res, err := p.List(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatalf("[#68-2] List: %v", err)
	}
	if len(res.IDs) != 0 {
		t.Errorf("[#68-2] chains outside the managed subset must not be listed; got %v", res.IDs)
	}
	t.Logf("[#68-2] PASS: Apply to a chain outside the managed subset is refused; file unchanged")
}
