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

// dnszone_unreadable_line_test.go は Backlog #37 の受け入れテスト。
//
// dnszone がレコード行として解釈できない行（打ち間違いなど）を黙って読み飛ばすと、
// そのレコードは「無い」と見なされて Create され、重複しうる。原則 3
// （unknown ≠ absent）に従い、読めない行が 1 行でもあればファイル全体を
// parse error にし、Read は Unknown、List はエラー（prune は halt）にする。
//
// AC tags:
//
//	[AC-37-1]  読めない行を含むゾーンで Read が Unknown（error）を返し、List がエラーを返す
//	[AC-37-2]  reconcile で prune が halt し、Create も Delete も出ず、ゾーンは書き換わらない
//	[AC-37-3]  コメント・空行・$ORIGIN / $TTL は従来どおり読める
package acceptance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/pkg/core"
)

// typoZone は keep / orphan の正しい行に加えて、打ち間違いの行を 1 行含む。
// 打ち間違いの行は本来 typo.example.com. の A レコード（"IN A" が "IN  A" でなく
// "INA" になっている）。
const typoZone = "; managed zone\n" +
	"$ORIGIN example.com.\n" +
	"$TTL 300\n" +
	"\n" +
	"keep.example.com. 300 IN A 1.1.1.1\n" +
	"orphan.example.com. 300 IN A 10.9.9.9\n" +
	"typo.example.com. 300 INA 10.0.0.7\n"

// TestDNSZone_UnreadableLine_ReadUnknownListError verifies [AC-37-1].
func TestDNSZone_UnreadableLine_ReadUnknownListError(t *testing.T) {
	// [AC-37-1]
	cases := map[string]string{
		"typo":          typoZone,
		"not a zone":    "keep.example.com. 300 IN A 1.1.1.1\nthis is { not a zone file\n",
		"multi-line":    "@ IN SOA ns1 admin (\n 2024010101 3600 900 604800 300 )\nkeep.example.com. 300 IN A 1.1.1.1\n",
		"$INCLUDE":      "$INCLUDE other.zone\nkeep.example.com. 300 IN A 1.1.1.1\n",
		"owner omitted": "keep.example.com. 300 IN A 1.1.1.1\n\t300 IN TXT \"v=spf1\"\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			zonePath := filepath.Join(t.TempDir(), "example.com.zone")
			writeZoneFile(t, zonePath, content)
			p := dnszone.New(zonePath)
			ctx := context.Background()

			// 正しく書かれた keep も含め、どのレコードも「無い」と言ってはならない。
			for _, id := range []string{"keep.example.com./A", "typo.example.com./A"} {
				obs, err := p.Read(ctx, id)
				if err == nil {
					t.Errorf("[AC-37-1] Read(%s) returned no error on an unreadable zone", id)
				}
				if obs.Availability != core.AvailabilityUnknown {
					t.Errorf("[AC-37-1] Read(%s) availability = %v, want Unknown (unknown ≠ absent)", id, obs.Availability)
				}
				if obs.State != nil {
					t.Errorf("[AC-37-1] Read(%s) returned state %v on an unreadable zone", id, obs.State)
				}
			}

			res, err := p.List(ctx, core.NewScope(nil))
			if err == nil {
				t.Fatalf("[AC-37-1] List returned no error on an unreadable zone (status %v, ids %v)", res.Status, res.IDs)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("[AC-37-1] List error does not name the unreadable line: %v", err)
			}
		})
	}
}

// TestDNSZone_UnreadableLine_PruneHalts verifies [AC-37-2] through the real
// sakutei binary.
func TestDNSZone_UnreadableLine_PruneHalts(t *testing.T) {
	// [AC-37-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zonePath := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zonePath, typoZone)
	snapshot := chaosStateBytes(t, zonePath)

	// intent: keep と typo（打ち間違いの行が表すはずのレコード）を PruneWithinScope で
	// 宣言する。読み飛ばせば typo は Create され（重複）、orphan は prune される。
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("keep.example.com./A", "keep.example.com.", "A", "1.1.1.1", "300", "PruneWithinScope"),
		dnszoneResource("typo.example.com./A", "typo.example.com.", "A", "10.0.0.7", "300", "PruneWithinScope"),
	})

	combined, _, code := runReconcileCombined(t, intentDir, zonePath)

	if code == 0 {
		t.Errorf("[AC-37-2] expected non-zero exit (zone unobservable this cycle); got 0\n%s", combined)
	}
	if !strings.Contains(combined, "prune halted") {
		t.Errorf("[AC-37-2] expected 'prune halted' (List error → complete-or-error); got:\n%s", combined)
	}
	if !strings.Contains(combined, "line 7") {
		t.Errorf("[AC-37-2] expected the error to name the unreadable line (line 7); got:\n%s", combined)
	}
	if strings.Contains(combined, "DELETE") || strings.Contains(combined, "CREATE") {
		t.Errorf("[AC-37-2] an op was emitted despite an unreadable zone:\n%s", combined)
	}
	if after := chaosStateBytes(t, zonePath); !bytes.Equal(snapshot, after) {
		t.Errorf("[AC-37-2] zone file changed under an unreadable zone\nbefore:\n%s\nafter:\n%s", snapshot, after)
	}
}

// TestDNSZone_SupportedLinesStillRead verifies [AC-37-3]: comments, blank lines,
// $ORIGIN / $TTL, single-line SOA/NS, quoted TXT with parentheses, and an
// indented comment do not make the zone unreadable.
func TestDNSZone_SupportedLinesStillRead(t *testing.T) {
	// [AC-37-3]
	zonePath := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zonePath, "; zone\n"+
		"$ORIGIN example.com.\n"+
		"$TTL 300\n"+
		"\n"+
		"   ; indented comment\n"+
		"@\t300 IN SOA ns1 admin 2024010101 3600 900 604800 300\n"+
		"@\t300 IN NS ns1.example.com.\n"+
		"www.example.com. 300 IN A 1.2.3.4\n"+
		"txt.example.com. 300 IN TXT \"a (b) c\"\n"+
		"mx.example.com. IN MX 10 mail.example.com. ; trailing (comment)\n"+
		"gen.example.com. 300 IN TYPE65534 \\# 0\n")
	p := dnszone.New(zonePath)

	res, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-37-3] List failed on a zone with only supported lines: %v", err)
	}
	if len(res.IDs) != 4 {
		t.Errorf("[AC-37-3] List returned %d ids, want 4 (www, txt, mx, gen): %v", len(res.IDs), res.IDs)
	}
	obs, err := p.Read(context.Background(), "www.example.com./A")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State == nil {
		t.Errorf("[AC-37-3] Read(www) = %+v, %v; want Ok with state", obs, err)
	}
}
