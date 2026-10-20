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

// chaos_v1a_test.go は Story Sc7a647-2（chaos スイート拡充・v1a マイルストーン）の
// 受け入れテストを保持する。既存の chaos_test.go（Scf7762-4）が fake provider
// 中心・budget/List gate 中心だったのに対し、本ファイルは
//
//   - 実 provider（dnszone / nftables）を実際の fault injection で揺さぶり、
//   - 安全不変条件（unknown≠absent／complete-or-error／budget halt／
//     ghost-diff 反復の非破壊ループ／部分 Apply 後の再観測収束／secret degrade）を、
//   - ログ行ではなく現実の状態（ファイルの bytes・resource 件数・apply_count 等の
//     実カウンタ）で証明する。
//
// fault injection は「production コードを弱めない」原則に従う。実 provider には
// テスト用の注入フックを足さず、provider が読む環境（ファイルの権限・型・親
// ディレクトリの書込可否・破損内容）を悪くすることで transient/parse エラーを
// 誘発する。fake provider は既存のテスト専用 knob（fail_inject / provider_version /
// apply_count）だけを使う。
//
// AC タグ（旧 ROADMAP.json（Backlog に移行済み） .sprints.Sc7a647.stories["Sc7a647-2"]）:
//
//	[AC-Sc7a647-2-1] provider 到達不能（Read が transient error / Unknown）→
//	                 unknown≠absent → 誤 prune なし。孤児は現実（zone/ruleset の
//	                 bytes）に残る。dnszone と nftables の両方で実証。加えて List
//	                 不完全（complete-or-error）が prune を halt することを両 provider
//	                 の現実で証明。
//	[AC-Sc7a647-2-2] 部分 Apply（一部 Op 成功・一部失敗）後、次サイクルの再観測で
//	                 残差だけが再計画され収束（観測 state レスの level 意味論）。別
//	                 シナリオで外部権威（secret/inventory の解決系）down 時に raw な
//	                 値が漏れず degrade して halt する。
//	[AC-Sc7a647-2-3] ghost diff の反復（provider 更新の holdループ）で無限の破壊的
//	                 Apply ループに陥らない（apply_count と現実値で証明）。budget 途中
//	                 枯渇で halt-all が残りの破壊的 Op を止める（実 dnszone prune の
//	                 現実 bytes で証明）。
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/render"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers (chaos-v1a 固有)
// ─────────────────────────────────────────────────────────────────────────────

// fakeApplyCount は fake provider の state ファイルから apply_count[localID] を読む。
// apply_count は Apply が呼ばれるたびに増える実カウンタ（テスト専用 knob）で、
// 「破壊的/修正 Apply が本当に走ったか」を現実の副作用として測る。
func fakeApplyCount(t *testing.T, statePath, localID string) int {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("fakeApplyCount: read %s: %v", statePath, err)
	}
	var sf struct {
		ApplyCount map[string]int `json:"apply_count"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("fakeApplyCount: unmarshal: %v", err)
	}
	return sf.ApplyCount[localID]
}

// fakeResourceSize は fake provider の state ファイルから resources[localID].fields.size を読む。
func fakeResourceSize(t *testing.T, statePath, localID string) string {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("fakeResourceSize: read %s: %v", statePath, err)
	}
	var sf struct {
		Resources map[string]struct {
			Fields map[string]string `json:"fields"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("fakeResourceSize: unmarshal: %v", err)
	}
	return sf.Resources[localID].Fields["size"]
}

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sc7a647-2-1] provider 到達不能 → unknown≠absent → 誤 prune なし（実 provider）
// ─────────────────────────────────────────────────────────────────────────────

// TestChaosV1a_DNSZoneUnreachable_NoFalsePrune verifies [AC-Sc7a647-2-1] against
// the REAL dnszone provider.
//
// 現実（zone ファイル）には desired なレコード keep と、intent に無い孤児
// orphan が実バイトとして存在する。intent は keep のみを PruneWithinScope で
// 宣言するので、正常なら orphan は prune 候補になる。ここでサイクル途中の
// 「provider 到達不能」を、zone ファイル「内容」を破損させることで実際に注入
// する（chmod でなく content corruption — root は DAC を bypass するので
// chmod では fault が効かない。nftables 兄弟テストと同じ content-corruption
// 方式に揃え、uid 非依存にする）。production コードは一切変更していない。
//
// 破損手法: 64KiB を超える改行なしの巨大 1 行を末尾に置く。dnszone の Read/List
// は bufio.Scanner で行読みするため、この行で bufio.ErrTooLong が scanner.Err()
// として発生し、parseZone がエラーを返す（os.ErrNotExist ではない）。これは
// ファイル権限に依存しないので root でも同じく発火する。keep/orphan の実バイトは
// ファイル先頭にそのまま残るので「孤児残存」も現実の bytes で証明できる。
//
// 期待される安全不変条件:
//   - Read は parse error → AvailabilityUnknown（unknown≠absent）。
//   - List も同じ parse error → complete-or-error により prune が halt。
//   - よって Delete は一切発行されず、孤児は現実に残る。
//
// 証明は「halt ログ」ではなく現実の bytes: reconcile 後、zone ファイルが
// 事前スナップショットと byte-for-byte 一致することを確認する（reconcile が
// zone を一切書き換えていない）。破損はテストが仕込んだファイル内容そのもの
// なので、検証のために fault を解く必要はない。
func TestChaosV1a_DNSZoneUnreachable_NoFalsePrune(t *testing.T) {
	// [AC-Sc7a647-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zonePath := filepath.Join(tmp, "example.com.zone")

	// 現実: keep + orphan の 2 レコードが実バイトで存在し、末尾に scanner を
	// 溢れさせる巨大 1 行（改行なし・64KiB 超）を置いて parse を失敗させる。
	// bufio.Scanner の既定 token 上限は 64KiB なので、余裕をもって 128KiB にする。
	overlong := strings.Repeat("x", 128*1024)
	initialZone := "; managed zone\n" +
		"keep.example.com. 300 IN A 1.1.1.1\n" +
		"orphan.example.com. 300 IN A 10.9.9.9\n" +
		overlong // 改行を付けない: 単一の巨大トークンとして ErrTooLong を誘発。
	writeZoneFile(t, zonePath, initialZone)
	snapshot := chaosStateBytes(t, zonePath)

	// intent: keep のみ（PruneWithinScope）。正常なら orphan は prune 候補。
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("keep.example.com./A", "keep.example.com.", "A", "1.1.1.1", "300", "PruneWithinScope"),
	})

	combined, _, code := runReconcileCombined(t, intentDir, zonePath)

	// D-1: 到達不能なので収束不完全 → exit 1（重要なのは Delete が出ないこと）。
	if code == 0 {
		t.Errorf("[AC-Sc7a647-2-1/dnszone] expected non-zero exit (unobservable this cycle); got 0\n%s", combined)
	}
	// prune は halt していなければならない（complete-or-error）。
	if !strings.Contains(combined, "prune halted") {
		t.Errorf("[AC-Sc7a647-2-1/dnszone] expected 'prune halted' (List error → complete-or-error); got:\n%s", combined)
	}
	// Read は Unknown/unobservable として扱われねばならない（unknown≠absent）。
	if !strings.Contains(combined, "unobservable") {
		t.Errorf("[AC-Sc7a647-2-1/dnszone] expected the record to be treated as unobservable (unknown≠absent); got:\n%s", combined)
	}
	// いかなる DELETE も発行されてはならない。
	if strings.Contains(combined, "DELETE") {
		t.Errorf("[AC-Sc7a647-2-1/dnszone] a DELETE op was emitted despite an unreachable/incomplete observation:\n%s", combined)
	}

	// 証明（現実の bytes）: zone は書き換わっていない → 孤児は残存。
	after := chaosStateBytes(t, zonePath)
	if !bytes.Equal(snapshot, after) {
		t.Errorf("[AC-Sc7a647-2-1/dnszone] DESTRUCTION LEAKED: zone file changed under an unreachable provider\nbefore:\n%s\nafter:\n%s",
			snapshot, after)
	}
	if !strings.Contains(string(after), "orphan.example.com.") {
		t.Errorf("[AC-Sc7a647-2-1/dnszone] orphan record was pruned despite unknown≠absent:\n%s", after)
	}
	t.Logf("[AC-Sc7a647-2-1/dnszone] PASS: unreachable dnszone → prune halted; orphan bytes preserved")
}

// TestChaosV1a_NFTablesUnreachable_NoFalsePrune verifies [AC-Sc7a647-2-1] against
// the REAL nftables provider.
//
// 現実（ruleset ファイル）には管理対象 chain "custom" と、intent に無い孤児
// chain "orphan" が実バイトで存在する。ここでファイル内容を破損させる（table
// ヘッダの外に chain を置く）ことで、parse を失敗させ Read→Unknown / List→error
// を実際に誘発する。これは provider が読む「悪い入力」による注入であり、
// production コードは変更していない。
//
// 期待: unknown≠absent かつ complete-or-error により prune halt。ruleset ファイルは
// byte-for-byte 不変（孤児 chain 残存）。
func TestChaosV1a_NFTablesUnreachable_NoFalsePrune(t *testing.T) {
	// [AC-Sc7a647-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	rulesetPath := filepath.Join(tmp, "chaos.rules")

	// 破損した ruleset: 先頭に「table の外の chain」を置いて parse を失敗させる。
	// ファイルには孤児 chain 相当のバイトも含む（現実の残存を証明するため）。
	corrupt := `chain orphan {
	ip saddr 10.9.9.9 accept
}
table ip filter {
	chain custom {
		ip saddr 10.0.0.1 accept
	}
}
`
	writeRulesetFile(t, rulesetPath, corrupt)
	snapshot := chaosStateBytes(t, rulesetPath)

	// intent: custom のみ（PruneWithinScope）。正常なら orphan は prune 候補。
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "nftables",
				"id":              "ip/filter/custom",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"family": "ip",
					"table":  "filter",
					"chain":  "custom",
					"rules":  nftEncoded("ip saddr 10.0.0.1 accept"),
				},
			},
		},
	})

	combined, _, code := runReconcileCombined(t, intentDir, rulesetPath)

	if code == 0 {
		t.Errorf("[AC-Sc7a647-2-1/nftables] expected non-zero exit (unobservable this cycle); got 0\n%s", combined)
	}
	if !strings.Contains(combined, "prune halted") {
		t.Errorf("[AC-Sc7a647-2-1/nftables] expected 'prune halted' (List parse error → complete-or-error); got:\n%s", combined)
	}
	if !strings.Contains(combined, "unobservable") {
		t.Errorf("[AC-Sc7a647-2-1/nftables] expected unobservable (unknown≠absent); got:\n%s", combined)
	}
	if strings.Contains(combined, "DELETE") {
		t.Errorf("[AC-Sc7a647-2-1/nftables] a DELETE op was emitted despite an incomplete/unknown observation:\n%s", combined)
	}

	// 証明（現実の bytes）: ruleset は書き換わっていない → 孤児 chain 残存。
	after := chaosStateBytes(t, rulesetPath)
	if !bytes.Equal(snapshot, after) {
		t.Errorf("[AC-Sc7a647-2-1/nftables] DESTRUCTION LEAKED: ruleset changed under an unreachable/parse-error provider\nbefore:\n%s\nafter:\n%s",
			snapshot, after)
	}
	if !strings.Contains(string(after), "chain orphan") {
		t.Errorf("[AC-Sc7a647-2-1/nftables] orphan chain was removed despite unknown≠absent:\n%s", after)
	}
	t.Logf("[AC-Sc7a647-2-1/nftables] PASS: parse-error nftables → prune halted; orphan chain bytes preserved")
}

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sc7a647-2-2] 部分 Apply 後の再観測収束（level-triggered self-heal）
// ─────────────────────────────────────────────────────────────────────────────

// TestChaosV1a_PartialApply_ReObserveConverges verifies [AC-Sc7a647-2-2] using the
// fake provider's fail_inject knob (existing test-only knob — no production change).
//
// 3 resource（vm-a, vm-b, vm-c）を Create する intent。fail_inject で vm-b の Apply
// だけを失敗させる（一部 Op 成功・一部失敗＝部分 Apply）。
//
// 不変条件（観測 state レスの level 意味論）:
//   - サイクル1: vm-a と vm-c は現実に作られ、vm-b は失敗して現実に存在しない
//     （＝ちょうど部分適用された現実が残る）。
//   - fail_inject を解いたサイクル2: エンジンは前サイクルの計画を持ち越さず現実を
//     再観測し、残差 vm-b だけを再計画して収束する。すでに存在する vm-a/vm-c は
//     再適用されない（apply_count で証明）。
func TestChaosV1a_PartialApply_ReObserveConverges(t *testing.T) {
	// [AC-Sc7a647-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// reality 空 + vm-b の Apply を失敗させる注入。
	sf := map[string]any{
		"resources":        map[string]any{},
		"provider_version": testProviderVersion,
		"fail_inject":      map[string]string{"vm-b": "injected disk full"},
		"apply_count":      map[string]int{},
	}
	writeJSONState(t, statePath, sf)

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "vm-a", "fields": map[string]string{"name": "vm-a", "size": "1"}},
			{"provider": "fake", "id": "vm-b", "fields": map[string]string{"name": "vm-b", "size": "2"}},
			{"provider": "fake", "id": "vm-c", "fields": map[string]string{"name": "vm-c", "size": "3"}},
		},
	})

	// サイクル1: 部分 Apply（vm-b が失敗）。
	combined1, _, _ := runReconcileCombined(t, intentDir, statePath)
	if !strings.Contains(combined1, "vm-b") || !strings.Contains(combined1, "injected disk full") {
		t.Errorf("[AC-Sc7a647-2-2] cycle1 expected an injected Apply failure for vm-b; got:\n%s", combined1)
	}
	// 現実は「ちょうど部分適用された」状態: vm-a, vm-c は存在、vm-b は不在。
	if !resourceExistsInState(t, statePath, "vm-a") || !resourceExistsInState(t, statePath, "vm-c") {
		t.Errorf("[AC-Sc7a647-2-2] cycle1: vm-a and vm-c must exist after partial apply")
	}
	if resourceExistsInState(t, statePath, "vm-b") {
		t.Errorf("[AC-Sc7a647-2-2] cycle1: vm-b must NOT exist (its Apply was injected to fail)")
	}
	// apply_count: vm-a, vm-c は 1 回ずつ適用済み。vm-b は Apply が呼ばれた（失敗）
	// ので count は増えるが現実には書かれていない — その基準値を記録しておく。
	if c := fakeApplyCount(t, statePath, "vm-a"); c != 1 {
		t.Errorf("[AC-Sc7a647-2-2] cycle1: vm-a apply_count = %d, want 1", c)
	}
	if c := fakeApplyCount(t, statePath, "vm-c"); c != 1 {
		t.Errorf("[AC-Sc7a647-2-2] cycle1: vm-c apply_count = %d, want 1", c)
	}
	vmbCountAfterCycle1 := fakeApplyCount(t, statePath, "vm-b")

	// fail_inject を解く（fault 復旧）。
	clearFakeFailInject(t, statePath)

	// サイクル2: 現実を再観測し、残差 vm-b だけを再計画して収束。
	combined2, _, code2 := runReconcileCombined(t, intentDir, statePath)
	if code2 != 0 {
		t.Errorf("[AC-Sc7a647-2-2] cycle2 expected exit 0 (converged); got %d\n%s", code2, combined2)
	}
	// 収束後、3 つとも現実に存在。
	for _, id := range []string{"vm-a", "vm-b", "vm-c"} {
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-Sc7a647-2-2] cycle2: %s missing after convergence", id)
		}
	}
	// level-triggered の核心: 既存の vm-a/vm-c は再適用されない（apply_count は 1 のまま）。
	if c := fakeApplyCount(t, statePath, "vm-a"); c != 1 {
		t.Errorf("[AC-Sc7a647-2-2] cycle2: vm-a was re-applied (apply_count=%d, want 1) — observation state was carried over instead of re-observed", c)
	}
	if c := fakeApplyCount(t, statePath, "vm-c"); c != 1 {
		t.Errorf("[AC-Sc7a647-2-2] cycle2: vm-c was re-applied (apply_count=%d, want 1)", c)
	}
	// 残差 vm-b はこのサイクルで（今度は成功して）ちょうど 1 回だけ再適用される。
	// fake の apply_count は失敗した Apply でも増えるため、cycle1 の失敗分を基準に
	// して「cycle2 で正確に 1 回だけ増えた」ことを検証する（残差のみ再計画）。
	if c := fakeApplyCount(t, statePath, "vm-b"); c != vmbCountAfterCycle1+1 {
		t.Errorf("[AC-Sc7a647-2-2] cycle2: vm-b apply_count = %d, want %d (exactly one residual re-plan)", c, vmbCountAfterCycle1+1)
	}
	// 出力上も「1 change applied」（残差だけ）であること。
	if !strings.Contains(combined2, "1 change(s) applied") {
		t.Errorf("[AC-Sc7a647-2-2] cycle2 expected exactly the residual (1 change applied); got:\n%s", combined2)
	}
	t.Logf("[AC-Sc7a647-2-2] PASS: partial apply → next cycle re-observed reality and converged only the residual (vm-a/vm-c not re-applied)")
}

// TestChaosV1a_DNSZoneApplyFails_ReObserveConverges verifies [AC-Sc7a647-2-2]
// against the REAL dnszone provider by making Apply fail via filesystem state.
//
// dnszone は temp+rename でアトミックに書く（writeLines: CreateTemp→Rename）。
// zone ファイルの親ディレクトリが「存在しない」とき、CreateTemp が失敗し
// Apply が transient error になる（provider コードは無傷、fault は「親 dir を
// 作らない」という filesystem の状態だけ）。read-only chmod は root が DAC を
// bypass するので効かないが、「dir が存在しない」は uid に一切依存しない
// （root でも存在しない dir には temp を作れない）ので root-robust である。
//
// このとき Read は os.Open(zonePath)→ENOENT（IsNotExist）を返すので現実は
// 「absent（空 zone）」として正しく観測される（unknown ではなく absent）。
// よって Create diff が立ち、Apply が走り、その write が親 dir 不在で失敗する。
// 次サイクルで親 dir を作ると、エンジンは現実（空の zone）を再観測して 2
// レコードを収束させる。level-triggered self-heal:「触っていないのに戻る／
// 壊れそうで止まる」を実 provider の zone bytes で証明。
func TestChaosV1a_DNSZoneApplyFails_ReObserveConverges(t *testing.T) {
	// [AC-Sc7a647-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	// zoneDir はサイクル1では「存在しない」— CreateTemp を uid 非依存に失敗させる。
	zoneDir := filepath.Join(tmp, "zonedir")
	zonePath := filepath.Join(zoneDir, "example.com.zone")

	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("a.example.com./A", "a.example.com.", "A", "1.1.1.1", "300", ""),
		dnszoneResource("b.example.com./A", "b.example.com.", "A", "2.2.2.2", "300", ""),
	})

	// fault injection: zone の親 dir を作らないことで Apply の temp 作成を失敗させる。
	combined1, _, _ := runReconcileCombined(t, intentDir, zonePath)

	if !strings.Contains(combined1, "create temp") && !strings.Contains(combined1, "no such file or directory") {
		t.Errorf("[AC-Sc7a647-2-2/dnszone] cycle1 expected an Apply failure (missing parent dir → CreateTemp fails); got:\n%s", combined1)
	}
	// 現実（zone）はまだ存在しない — レコードは書かれていない。
	if _, err := os.Stat(zonePath); !os.IsNotExist(err) {
		t.Errorf("[AC-Sc7a647-2-2/dnszone] cycle1: zone file must not exist yet (Apply failed); stat err=%v", err)
	}

	// 復旧（後続の収束のため）: 親 dir を作ると write が通るようになる。
	if err := os.MkdirAll(zoneDir, 0755); err != nil {
		t.Fatal(err)
	}

	// サイクル2: dir 出現 → 現実を再観測して収束。
	combined2, _, code2 := runReconcileCombined(t, intentDir, zonePath)
	if code2 != 0 {
		t.Errorf("[AC-Sc7a647-2-2/dnszone] cycle2 expected exit 0 (converged); got %d\n%s", code2, combined2)
	}
	zoneContent := readZoneFile(t, zonePath)
	if !strings.Contains(zoneContent, "a.example.com.") || !strings.Contains(zoneContent, "b.example.com.") {
		t.Errorf("[AC-Sc7a647-2-2/dnszone] cycle2: zone did not converge to both records:\n%s", zoneContent)
	}
	t.Logf("[AC-Sc7a647-2-2/dnszone] PASS: Apply failed under missing parent dir; next cycle re-observed and converged both records")
}

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sc7a647-2-2] secret/外部権威 down → degrade（縮退）・漏洩なし
// ─────────────────────────────────────────────────────────────────────────────

// TestChaosV1a_SecretBackendDown_DegradesNoLeak verifies [AC-Sc7a647-2-2] at the
// library level: when the file SecretBackend store is unreachable or malformed,
// GenerationOf / ResolveValue must FAIL (degrade) rather than return a value, and
// the error must NEVER contain the plaintext canary (§20 defense-in-depth).
//
// fault injection: store ファイルを (a) 存在させない（unreachable）と (b) 破損
// JSON（中に canary 平文を含む）にする。どちらも provider の悪い入力/欠落で、
// production コードは無傷。
func TestChaosV1a_SecretBackendDown_DegradesNoLeak(t *testing.T) {
	// [AC-Sc7a647-2-2]
	const canary = "CHAOS-SECRET-CANARY-9f3a"
	tmp := t.TempDir()

	// (a) store ファイルが存在しない → unreachable として degrade。
	missing := filepath.Join(tmp, "does-not-exist.json")
	be := render.NewFileSecretBackend(missing)
	if _, err := be.GenerationOf(context.Background(), "db/root"); err == nil {
		t.Errorf("[AC-Sc7a647-2-2/secret] missing store: GenerationOf must return an error (degrade), got nil")
	} else if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("[AC-Sc7a647-2-2/secret] missing store: error should say 'unreachable'; got %q", err.Error())
	}

	// (b) store が破損 JSON で、破損部に canary 平文を含む。raw parser error は入力を
	//     引用しうる — degrade error は canary を絶対に晒してはならない。
	corruptPath := filepath.Join(tmp, "store.json")
	corrupt := `{"secrets": {"db/root": {"gen": 1, "value": "` + canary + `" MISSING_COMMA }}}`
	if err := os.WriteFile(corruptPath, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	be2 := render.NewFileSecretBackend(corruptPath)

	genOut, genErr := be2.GenerationOf(context.Background(), "db/root")
	if genErr == nil {
		t.Fatalf("[AC-Sc7a647-2-2/secret] corrupt store: GenerationOf must error (degrade), got value %q", genOut)
	}
	// degrade error は canary を含んではならない（canary-absent）。
	if strings.Contains(genErr.Error(), canary) {
		t.Fatalf("[AC-Sc7a647-2-2/secret] SECRET LEAK: GenerationOf error surfaced the plaintext canary: %q", genErr.Error())
	}
	// GenerationOf は成功時でも値を返さない（metadata only）。ここでは失敗しているが、
	// 返り値にも canary が無いことを念のため確認。
	if strings.Contains(genOut, canary) {
		t.Fatalf("[AC-Sc7a647-2-2/secret] SECRET LEAK: GenerationOf return value contained the canary")
	}

	// ResolveValue（JIT 実値化）も同様に degrade し、error に canary を晒さない。
	valOut, valErr := be2.ResolveValue(context.Background(), "file", "db/root", 1)
	if valErr == nil {
		t.Fatalf("[AC-Sc7a647-2-2/secret] corrupt store: ResolveValue must error (degrade), got %q", valOut)
	}
	if strings.Contains(valErr.Error(), canary) {
		t.Fatalf("[AC-Sc7a647-2-2/secret] SECRET LEAK: ResolveValue error surfaced the plaintext canary: %q", valErr.Error())
	}
	t.Logf("[AC-Sc7a647-2-2/secret] PASS: secret backend down/corrupt → degrade (error), no plaintext canary leaked")
}

// TestChaosV1a_ExternalAuthorityDown_HaltSafeNoLeak verifies [AC-Sc7a647-2-2] at
// the CLI level against the REAL dnszone provider: when the external authority
// (render phase 1 resolver) is unreachable on the first call with no prior cached
// value, "sakutei reconcile" must HALT-safe (exit 1) — it must NOT proceed with an
// empty desired and must NOT write to reality (§9.2 last-value-on-unreachable).
//
// fault injection: --inventory-unreachable フラグ（テスト用 FakeInventoryResolver の
// Fail=true）。これは phase1 の外部権威 down を模す既存の seam であり、provider の
// production コードは無傷。zone は byte-for-byte 不変であることで「空 desired を
// 出して破壊しない」を証明する。
func TestChaosV1a_ExternalAuthorityDown_HaltSafeNoLeak(t *testing.T) {
	// [AC-Sc7a647-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zonePath := filepath.Join(tmp, "example.com.zone")
	initialZone := `; managed zone
keep.example.com. 300 IN A 1.1.1.1
`
	writeZoneFile(t, zonePath, initialZone)
	snapshot := chaosStateBytes(t, zonePath)

	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("keep.example.com./A", "keep.example.com.", "A", "1.1.1.1", "300", ""),
	})

	// fault injection: 外部権威を到達不能に（初回・prior cache 無し）。
	combined, _, code := runReconcileCombined(t, intentDir, zonePath, "--inventory-unreachable")

	// halt-safe: exit 1、収束は前進させない。
	if code == 0 {
		t.Errorf("[AC-Sc7a647-2-2/external] expected exit 1 (halt-safe); got 0\n%s", combined)
	}
	if !strings.Contains(combined, "ExternalInputUnreachable") {
		t.Errorf("[AC-Sc7a647-2-2/external] expected ExternalInputUnreachable event; got:\n%s", combined)
	}
	if !strings.Contains(combined, "halt-safe") {
		t.Errorf("[AC-Sc7a647-2-2/external] expected halt-safe messaging (§9.2); got:\n%s", combined)
	}
	// 空 desired で進んで書き換えていないこと（現実の bytes で証明）。
	after := chaosStateBytes(t, zonePath)
	if !bytes.Equal(snapshot, after) {
		t.Errorf("[AC-Sc7a647-2-2/external] reality changed while external authority was down (empty-desired leak?)\nbefore:\n%s\nafter:\n%s",
			snapshot, after)
	}
	t.Logf("[AC-Sc7a647-2-2/external] PASS: external authority down → halt-safe; zone bytes preserved (no empty-desired destruction)")
}

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sc7a647-2-3] ghost diff 反復 → 無限破壊ループなし（daemon, apply_count で証明）
// ─────────────────────────────────────────────────────────────────────────────

// TestChaosV1a_RepeatedGhostDiff_NoDestructiveLoop verifies [AC-Sc7a647-2-3]:
// a repeatedly-detected ghost diff (cause C, provider_version bumped while
// reality drifted) must be HELD every cycle in the daemon and must NEVER trigger
// a destructive/corrective Apply loop.
//
// 手順（決定的、実 sleep 依存を最小化しつつログでゲート）:
//  1. reality を intent と一致・provider_version=v1 に置いて daemon を起動 →
//     cycle 1 で in-memory baseline を v1 に確立（cause C は出ない）。
//  2. baseline 確立を "cycle 1:" のログで待ってから、provider_version を v2 に
//     bump し、同時に reality を drift（size 10→11）させる（原子的に 1 回書き込む）。
//     以後、毎サイクル同じ diff が cause C（ghost diff）として分類され held される。
//  3. ProviderUpgradeDiff が複数サイクルにわたり繰り返し出ることを確認（＝振動的な
//     反復 diff が封じ込められ、hold し続けている）。
//
// 証明（現実の実カウンタ）:
//   - reality の size は 11 のまま（ghost diff は決して適用されない）。
//   - fake の apply_count[dc-vm] は 0（破壊的/修正 Apply が 1 度も走っていない）。
//     → 無限破壊ループに陥っていないことを、ログ行ではなく実カウンタで示す。
func TestChaosV1a_RepeatedGhostDiff_NoDestructiveLoop(t *testing.T) {
	// [AC-Sc7a647-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	// converged reality at v1: dc-vm size=10, intent size=10.
	writeJSONState(t, statePath, map[string]any{
		"resources": map[string]any{
			"dc-vm": map[string]any{"fields": map[string]string{"name": "dc-vm", "size": "10"}},
		},
		"provider_version": "v1.0.0",
		"apply_count":      map[string]int{},
	})
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "dc-vm", "fields": map[string]string{"name": "dc-vm", "size": "10"}},
		},
	})

	cmd, waitCmd := startDaemonToFile(t, intentDir, statePath, "150ms", logPath)
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()

	// cycle 1 完了を待って baseline(v1) が確立されるのを保証（原子的な race 回避）。
	if !waitForLogFile(logPath, "cycle 1:", 5*time.Second) {
		t.Fatalf("[AC-Sc7a647-2-3] daemon did not complete cycle 1; log:\n%s", readFileString(logPath))
	}

	// provider_version bump + reality drift を 1 回の書き込みで注入（B 誤分類の窓を潰す）。
	writeJSONState(t, statePath, map[string]any{
		"resources": map[string]any{
			"dc-vm": map[string]any{"fields": map[string]string{"name": "dc-vm", "size": "11"}},
		},
		"provider_version": "v2.0.0",
		"apply_count":      map[string]int{},
	})

	// ghost diff が繰り返し held されること（振動的反復 diff の封じ込め）を待つ。
	if !waitForLogFile(logPath, "ProviderUpgradeDiff", 5*time.Second) {
		t.Fatalf("[AC-Sc7a647-2-3] expected a ProviderUpgradeDiff (cause C) after version bump; log:\n%s", readFileString(logPath))
	}
	// 複数サイクルにわたり held し続けることを確認するため少し回してから止める。
	if !waitForLogFileCount(logPath, "ProviderUpgradeDiff", 3, 5*time.Second) {
		t.Fatalf("[AC-Sc7a647-2-3] expected the ghost diff to be HELD across multiple cycles (>=3 ProviderUpgradeDiff); log:\n%s",
			readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true

	log := readFileString(logPath)

	// 証明1（現実値）: ghost diff は決して適用されない → size は 11 のまま。
	if got := fakeResourceSize(t, statePath, "dc-vm"); got != "11" {
		t.Errorf("[AC-Sc7a647-2-3] reality size = %q, want 11 (ghost diff must be HELD, never applied); log:\n%s", got, log)
	}
	// 証明2（実カウンタ）: 破壊的/修正 Apply は 1 度も走っていない → apply_count == 0。
	if c := fakeApplyCount(t, statePath, "dc-vm"); c != 0 {
		t.Errorf("[AC-Sc7a647-2-3] dc-vm apply_count = %d, want 0 — a repeated ghost diff must NOT drive an Apply loop; log:\n%s", c, log)
	}
	// [cause: C] であることも確認（B に誤分類して自動適用していない）。
	if !strings.Contains(log, "[cause: C]") {
		t.Errorf("[AC-Sc7a647-2-3] expected the held diff to be classified [cause: C]; log:\n%s", log)
	}
	t.Logf("[AC-Sc7a647-2-3] PASS: repeated ghost diff held across cycles; reality unchanged (size=11) and apply_count=0 (no destructive loop)")
}

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sc7a647-2-3] budget 途中枯渇 → halt-all で実 dnszone prune を止める
// ─────────────────────────────────────────────────────────────────────────────

// TestChaosV1a_DNSZoneBudgetHalt_NoDestruction verifies [AC-Sc7a647-2-3] against
// the REAL dnszone provider, whose Delete is a genuine destructive op (dnszone is
// Pruneable=true, unlike nftables).
//
// 現実の zone に keep + 4 孤児レコードがある。intent は keep のみ
// (PruneWithinScope) なので 4 件の Delete が計画される。--max-destructive-per-cycle=2
// と --on-exceed=halt-all を与えると、4 > 2 で budget が halt-all を発火し、残りの
// 破壊的 Op は 1 つも実行されない。
//
// 証明（現実の bytes）: zone は事前スナップショットと byte-for-byte 一致
// （どの孤児も削除されていない）。
func TestChaosV1a_DNSZoneBudgetHalt_NoDestruction(t *testing.T) {
	// [AC-Sc7a647-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zonePath := filepath.Join(tmp, "example.com.zone")

	initialZone := `; managed zone
keep.example.com. 300 IN A 1.1.1.1
o1.example.com. 300 IN A 10.0.0.1
o2.example.com. 300 IN A 10.0.0.2
o3.example.com. 300 IN A 10.0.0.3
o4.example.com. 300 IN A 10.0.0.4
`
	writeZoneFile(t, zonePath, initialZone)
	snapshot := chaosStateBytes(t, zonePath)

	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("keep.example.com./A", "keep.example.com.", "A", "1.1.1.1", "300", "PruneWithinScope"),
	})

	combined, _, code := runReconcileCombined(t, intentDir, zonePath,
		"--max-destructive-per-cycle=2",
		"--on-exceed=halt-all",
	)

	// budget が halt-all を発火（4 > 2）。
	if code == 0 {
		t.Errorf("[AC-Sc7a647-2-3/dnszone] expected non-zero exit (budget halted); got 0\n%s", combined)
	}
	if !strings.Contains(combined, "BUDGET HALTED") {
		t.Errorf("[AC-Sc7a647-2-3/dnszone] expected 'BUDGET HALTED' (4 destructive > max 2); got:\n%s", combined)
	}

	// 証明（現実の bytes）: halt-all は最初の Op の前に止まる → zone は不変。
	after := chaosStateBytes(t, zonePath)
	if !bytes.Equal(snapshot, after) {
		t.Errorf("[AC-Sc7a647-2-3/dnszone] DESTRUCTION LEAKED: zone changed despite halt-all\nbefore:\n%s\nafter:\n%s",
			snapshot, after)
	}
	for _, id := range []string{"o1.example.com.", "o2.example.com.", "o3.example.com.", "o4.example.com."} {
		if !strings.Contains(string(after), id) {
			t.Errorf("[AC-Sc7a647-2-3/dnszone] orphan %s was deleted despite budget halt-all:\n%s", id, after)
		}
	}
	t.Logf("[AC-Sc7a647-2-3/dnszone] PASS: real dnszone prune halted by budget (halt-all); all orphan records preserved byte-for-byte")
}

// ─────────────────────────────────────────────────────────────────────────────
// shared helpers introduced by this file
// ─────────────────────────────────────────────────────────────────────────────

// writeJSONState marshals sf and writes it to statePath (0600).
func writeJSONState(t *testing.T, statePath string, sf map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("writeJSONState: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("writeJSONState: write: %v", err)
	}
}

// clearFakeFailInject removes the fail_inject map from a fake-provider state file
// (fault recovery: subsequent Apply calls succeed again).
func clearFakeFailInject(t *testing.T, statePath string) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("clearFakeFailInject: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("clearFakeFailInject: unmarshal: %v", err)
	}
	delete(sf, "fail_inject")
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("clearFakeFailInject: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("clearFakeFailInject: write: %v", err)
	}
}

// waitForLogFileCount polls logPath until substr appears at least n times or the
// deadline passes.  Returns true on match.  Used to prove a ghost diff is HELD
// across MULTIPLE cycles (not merely detected once).
func waitForLogFileCount(logPath, substr string, n int, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Count(string(data), substr) >= n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
