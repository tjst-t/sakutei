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

// statusreport_test.go — report-builder 層の単体テスト（Story Sc7a647-3）。
//
// ここでは特に last-seen「前周は X だった」機構をリング層で検証する: 観測を 2 回
// リングへ与え、レポートが前周スナップショットの要約を出すことを assert する。
// CLI の単一実行では前周が無いため、機構はこの report-builder 層でテストするのが
// 正しい（CLI で偽の前周をでっち上げない）。
package statusreport

import (
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/lastseen"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// TestReport_LastSeenPreviousSnapshot verifies the "前周は X だった" mechanism at
// the report-builder level: feeding a previous + current observation into the
// ring makes the report show the previous snapshot's field value.
//
// [AC-Sc7a647-3-3]
func TestReport_LastSeenPreviousSnapshot(t *testing.T) {
	// [AC-Sc7a647-3-3]
	ring := lastseen.NewResourceRing(lastseen.DefaultRingSize)

	// 前周観測: size=10。
	ring.Record("vm-1", lastseen.Snapshot{
		LocalID:    "vm-1",
		ObservedAt: time.Unix(1000, 0),
		Fields:     map[string]string{"size": "10"},
	})

	// 今周観測: size=20（drift）。builder が今周フィールドを渡してリングと比較する。
	b := NewBuilder("rev-int", "rev-rendered", ring)
	tier := TierFromCapabilities(provider.Capabilities{TrueObserve: true, ProviderVersion: "v1"})
	b.AddDrifted(
		core.ID{Provider: "fake", Local: "vm-1"},
		core.CauseTagB,
		tier,
		"size out of sync",
		map[string]string{"size": "20"},
	)
	report := b.Build()

	if len(report.Drifted) != 1 {
		t.Fatalf("[AC-Sc7a647-3-3] expected 1 drifted item, got %d", len(report.Drifted))
	}
	ls := report.Drifted[0].LastSeen
	// 前周は 10, 今周は 20 —— 要約に前周値 10 と今周値 20 が現れる。
	if !strings.Contains(ls, "10") || !strings.Contains(ls, "20") {
		t.Errorf("[AC-Sc7a647-3-3] last-seen summary should show prev=10 and cur=20; got: %q", ls)
	}
	if !strings.Contains(ls, "prev") && !strings.Contains(ls, "drift") {
		t.Errorf("[AC-Sc7a647-3-3] last-seen summary should reference the previous cycle; got: %q", ls)
	}
	t.Logf("[AC-Sc7a647-3-3] last-seen 前周表示: %s", ls)
}

// TestReport_LastSeenNoPreviousOnFirstRun verifies that with an empty ring
// (first run / post-restart, S3 non-persistent), the report shows a
// "no previous snapshot" note rather than fabricating a previous cycle.
//
// [AC-Sc7a647-3-3]
func TestReport_LastSeenNoPreviousOnFirstRun(t *testing.T) {
	// [AC-Sc7a647-3-3]
	ring := lastseen.NewResourceRing(lastseen.DefaultRingSize)

	b := NewBuilder("rev-int", "rev-rendered", ring)
	tier := TierFromCapabilities(provider.Capabilities{TrueObserve: true, ProviderVersion: "v1"})
	b.AddDrifted(
		core.ID{Provider: "fake", Local: "vm-new"},
		core.CauseTagA,
		tier,
		"absent in reality",
		map[string]string{"size": "5"},
	)
	report := b.Build()

	ls := report.Drifted[0].LastSeen
	if !strings.Contains(ls, "no previous snapshot") {
		t.Errorf("[AC-Sc7a647-3-3] first-run empty ring should say 'no previous snapshot'; got: %q", ls)
	}
	t.Logf("[AC-Sc7a647-3-3] first-run last-seen: %s", ls)
}

// TestReport_NilRingIsNoPrevious verifies that a nil ring yields the Japanese
// "前周スナップショットなし" note (belt-and-suspenders for callers that pass no ring).
//
// [AC-Sc7a647-3-3]
func TestReport_NilRingIsNoPrevious(t *testing.T) {
	// [AC-Sc7a647-3-3]
	b := NewBuilder("rev-int", "rev-rendered", nil)
	tier := TierFromCapabilities(provider.Capabilities{ProviderVersion: "v1"})
	b.AddDrifted(core.ID{Provider: "fake", Local: "x"}, core.CauseTagB, tier, "", nil)
	report := b.Build()
	if !strings.Contains(report.Drifted[0].LastSeen, "前周スナップショットなし") {
		t.Errorf("[AC-Sc7a647-3-3] nil ring should note 前周スナップショットなし; got: %q", report.Drifted[0].LastSeen)
	}
}

// TestReport_TierAndCauseAndReadOnlyText verifies the report surfaces the
// provider tier and cause tag, and that the rendered text contains NO Apply-style
// verbs (CREATE/UPDATE/DELETE/applied) — the read-only text property.
//
// [AC-Sc7a647-3-1] [AC-Sc7a647-3-2] [AC-Sc7a647-3-3]
func TestReport_TierAndCauseAndReadOnlyText(t *testing.T) {
	// [AC-Sc7a647-3-1] [AC-Sc7a647-3-3]
	ring := lastseen.NewResourceRing(lastseen.DefaultRingSize)
	b := NewBuilder("rev-int", "rev-rendered", ring)
	tier := TierFromCapabilities(provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		BulkObserve:     false,
		Pruneable:       true,
		ProviderVersion: "v-dnszone-1",
	})
	b.AddDrifted(core.ID{Provider: "dnszone", Local: "www"}, core.CauseTagB, tier, "value out of sync", map[string]string{"value": "1.2.3.4"})
	b.AddConverged()
	report := b.Build()
	out := report.String()

	// tier 表示。
	if !strings.Contains(out, "TrueObserve=true") || !strings.Contains(out, "ProviderVersion=v-dnszone-1") {
		t.Errorf("[AC-Sc7a647-3-3] report should show provider tier; got:\n%s", out)
	}
	// 原因タグ。
	if !strings.Contains(out, "B（ドリフト）") {
		t.Errorf("[AC-Sc7a647-3-1] report should show cause tag B; got:\n%s", out)
	}
	// converged 数。
	if !strings.Contains(out, "converged=1") {
		t.Errorf("[AC-Sc7a647-3-1] report should count converged=1; got:\n%s", out)
	}

	// [AC-Sc7a647-3-2] read-only text: no Apply-style execution verbs.
	for _, forbidden := range []string{"CREATE", "UPDATE", "DELETE", "applied", "Applying"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("[AC-Sc7a647-3-2] report text must NOT contain Apply verb %q (read-only); got:\n%s", forbidden, out)
		}
	}
}

// TestReport_CauseDPropagation verifies a downstream (cause D) drifted item is
// surfaced in the report with the D label.
//
// [AC-Sc7a647-3-3]
func TestReport_CauseDPropagation(t *testing.T) {
	// [AC-Sc7a647-3-3]
	b := NewBuilder("rev-int", "rev-rendered", lastseen.NewResourceRing(lastseen.DefaultRingSize))
	tier := TierFromCapabilities(provider.Capabilities{TrueObserve: true, ProviderVersion: "v1"})
	b.AddDrifted(core.ID{Provider: "fake", Local: "child"}, core.CauseTagD, tier, "upstream changed", map[string]string{"x": "1"})
	report := b.Build()
	if !strings.Contains(report.String(), "D（波及") {
		t.Errorf("[AC-Sc7a647-3-3] downstream cause D should appear; got:\n%s", report.String())
	}
}

// TestReport_UnmanagedSection verifies the unmanaged（宣言外・実在）section
// renders each undeclared reality ID, the "not deleted" note, and unmanaged=N in
// the summary (FIX #2, Sc7a647-3).
//
// [AC-Sc7a647-3-unmanaged]
func TestReport_UnmanagedSection(t *testing.T) {
	// [AC-Sc7a647-3-unmanaged]
	b := NewBuilder("rev-int", "rev-rendered", nil)
	b.AddConverged()
	b.AddUnmanaged(core.ID{Provider: "fake", Local: "rogue-1"})
	b.AddUnmanaged(core.ID{Provider: "fake", Local: "rogue-2"})
	out := b.Build().String()

	if !strings.Contains(out, "unmanaged（宣言外・実在・prune 対象外）: 2 リソース") {
		t.Errorf("[AC-Sc7a647-3-unmanaged] expected unmanaged heading with count 2; got:\n%s", out)
	}
	if !strings.Contains(out, "rogue-1") || !strings.Contains(out, "rogue-2") {
		t.Errorf("[AC-Sc7a647-3-unmanaged] both undeclared IDs must be listed; got:\n%s", out)
	}
	if !strings.Contains(out, "自動削除されない") {
		t.Errorf("[AC-Sc7a647-3-unmanaged] must carry the not-auto-deleted note; got:\n%s", out)
	}
	if !strings.Contains(out, "unmanaged=2") {
		t.Errorf("[AC-Sc7a647-3-unmanaged] summary must show unmanaged=2; got:\n%s", out)
	}
}

// TestReport_UnmanagedUndetectableNote verifies the graceful-degradation note
// path (a provider that cannot enumerate reality — e.g. a non-Lister provider or
// a failed/incomplete List) is surfaced as a note and does NOT inflate the
// unmanaged count (unmanaged stays 0: honest, not fabricated).
//
// This is the report-builder proof for the non-Lister degradation branch, which
// is not reachable via the CLI (every CLI-registered provider implements Lister).
//
// [AC-Sc7a647-3-unmanaged]
func TestReport_UnmanagedUndetectableNote(t *testing.T) {
	// [AC-Sc7a647-3-unmanaged]
	b := NewBuilder("rev-int", "rev-rendered", nil)
	b.AddConverged()
	b.NoteUnmanagedUndetectable("unmanaged 検出不可（List 非対応 provider: noimpl）")
	out := b.Build().String()

	if !strings.Contains(out, "unmanaged 検出不可（List 非対応 provider: noimpl）") {
		t.Errorf("[AC-Sc7a647-3-unmanaged] expected the non-Lister degradation note; got:\n%s", out)
	}
	// Degradation must not fabricate unmanaged entries.
	if !strings.Contains(out, "unmanaged=0") {
		t.Errorf("[AC-Sc7a647-3-unmanaged] undetectable must keep unmanaged=0 (not fabricated); got:\n%s", out)
	}
	if strings.Contains(out, "UNMANAGED ") {
		t.Errorf("[AC-Sc7a647-3-unmanaged] no UNMANAGED entries should be listed; got:\n%s", out)
	}
}

// TestReport_IncompleteOnUnobservableOrListError (#117): a resource that could
// not be observed, or a provider whose List failed, makes the report
// Incomplete (status exits 3); a plain drift does not.
//
// [AC-114-3-1]
func TestReport_IncompleteOnUnobservableOrListError(t *testing.T) {
	id := core.ID{Provider: "fake", Local: "bmc-1"}

	b := NewBuilder("r1", "r1", nil)
	b.AddDrifted(id, core.CauseTagB, Tier{}, "", nil)
	if b.Build().Incomplete() {
		t.Errorf("a drift alone must not make the report incomplete")
	}

	b = NewBuilder("r1", "r1", nil)
	b.AddUnobservableReason(id, "observe error: boom")
	r := b.Build()
	if !r.Incomplete() || r.UnobservableCount != 1 || len(r.Unobservable) != 1 {
		t.Fatalf("unobservable resource should make the report incomplete: %+v", r)
	}
	if out := r.String(); !strings.Contains(out, "UNOBSERVABLE fake//bmc-1") || !strings.Contains(out, "observe error: boom") {
		t.Errorf("report should list the unobservable resource with its reason; got:\n%s", out)
	}

	b = NewBuilder("r1", "r1", nil)
	b.NoteListError("unmanaged 検出不可（List エラー: fake: boom）")
	r = b.Build()
	if !r.Incomplete() || r.ListErrorCount != 1 {
		t.Errorf("a List error should make the report incomplete: %+v", r)
	}
	if !strings.Contains(r.String(), "List エラー: fake: boom") {
		t.Errorf("List error note should be shown; got:\n%s", r.String())
	}
}
