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

// Package acceptance — Backlog #70: dnszone の SOA シリアル（設計書 v3.12 §29.1）。
//
// 管理するレコードを書き換えた Apply だけが、同じ書き込みで SOA のシリアルを
// max(今日の YYYYMMDD00〈UTC〉, 今の値 + 1) に上げる。SOA のほかの欄と行の書き方は
// 変えない。変更の無い Apply・keep の設定では上げない。上限を超えるなら止まる。
//
// AC タグ:
//
//	[AC-70-1] 変えた Apply はシリアルを上げ、SOA のほかの欄と行の書き方（括弧で
//	          複数行に分けた SOA を含む）は変えない。変えない Apply は上げない
//	[AC-70-2] 新しい値 = max(今日の YYYYMMDD00, 今の値 + 1)：同じ日の 2 回目は nn が
//	          進み、日付の形でない値・未来の日付は + 1。上限超えは PermanentError
//	[AC-70-3] keep では上げない。SOA が無いゾーンはそのまま書く
//	[AC-70-4] CLI：reconcile がシリアルを上げ、次の plan は Converged。
//	          --zone-soa-serial keep では上げない。不正な値は起動時にエラー
package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// soaDay は差し替える時計の「今日」（UTC）。
var soaDay = time.Date(2026, 10, 21, 23, 30, 0, 0, time.UTC)

func soaClock() time.Time { return soaDay }

func soaZone(serial string) string {
	return "; example\n" +
		"$ORIGIN example.com.\n" +
		"$TTL 300\n" +
		"@\t300 IN SOA ns1 admin " + serial + " 3600 900 604800 300 ; serial\n" +
		"@\t300 IN NS ns1\n" +
		"www 300 IN A 192.0.2.1\n"
}

func soaUpdate(value string) core.Plan {
	return core.Plan{Ops: []core.Op{{
		Kind:   core.OpKindUpdate,
		Target: core.NewID(dnszone.ProviderName, core.NewScope(nil), "www.example.com./A"),
		Fields: []core.FieldDiff{{Field: "value", From: "192.0.2.1", To: value}},
	}}}
}

var soaSerialRe = regexp.MustCompile(`SOA ns1 admin (\d+) `)

func soaSerialOf(t *testing.T, zone string) string {
	t.Helper()
	m := soaSerialRe.FindStringSubmatch(zone)
	if m == nil {
		t.Fatalf("no SOA serial in zone:\n%s", zone)
	}
	return m[1]
}

// TestDNSZone_SOASerial_BumpOnChangeOnly verifies [AC-70-1].
func TestDNSZone_SOASerial_BumpOnChangeOnly(t *testing.T) {
	// [AC-70-1]
	ctx := context.Background()
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, soaZone("2024010101"))
	p := dnszone.New(zone, dnszone.WithClock(soaClock))

	if err := p.Apply(ctx, soaUpdate("192.0.2.50")); err != nil {
		t.Fatalf("[AC-70-1] Apply: %v", err)
	}
	want := strings.Replace(soaZone("2026102100"), "192.0.2.1", "192.0.2.50", 1)
	if got := readZoneFile(t, zone); got != want {
		t.Errorf("[AC-70-1] zone after a changing Apply:\n%s\nwant (only the serial and the record differ):\n%s", got, want)
	}

	// 同じ値の Update・無いレコードの Delete はファイルを変えない → シリアルも上げない。
	before := readZoneFile(t, zone)
	if err := p.Apply(ctx, soaUpdate("192.0.2.50")); err != nil {
		t.Fatalf("[AC-70-1] no-op Update: %v", err)
	}
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete,
		Target: core.NewID(dnszone.ProviderName, core.NewScope(nil), "gone.example.com./A")}}}); err != nil {
		t.Fatalf("[AC-70-1] no-op Delete: %v", err)
	}
	if got := readZoneFile(t, zone); got != before {
		t.Errorf("[AC-70-1] an Apply that changes nothing changed the zone:\n%s\nwant:\n%s", got, before)
	}

	// 括弧で複数行に分けた SOA：シリアルの数字だけが変わり、行の書き方は残る。
	multi := "$ORIGIN example.com.\n" +
		"@ 3600 IN SOA ns1.example.com. hostmaster.example.com. (\n" +
		"\t\t2026102105 ; serial\n" +
		"\t\t7200       ; refresh\n" +
		"\t\t3600 1209600\n" +
		"\t\t300 )      ; minimum\n" +
		"@ IN NS ns1.example.com.\n" +
		"www 300 IN A 192.0.2.1\n"
	writeZoneFile(t, zone, multi)
	if ids := listLocals(t, p); len(ids) != 1 || ids[0] != "www.example.com./A" {
		t.Fatalf("[AC-70-1] List on a zone with a multi-line SOA = %v, want [www.example.com./A]", ids)
	}
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete,
		Target: core.NewID(dnszone.ProviderName, core.NewScope(nil), "www.example.com./A")}}}); err != nil {
		t.Fatalf("[AC-70-1] Delete with a multi-line SOA: %v", err)
	}
	want = strings.Replace(multi, "2026102105", "2026102106", 1)
	want = strings.Replace(want, "www 300 IN A 192.0.2.1\n", "", 1)
	if got := readZoneFile(t, zone); got != want {
		t.Errorf("[AC-70-1] multi-line SOA after Delete:\n%s\nwant:\n%s", got, want)
	}
}

// TestDNSZone_SOASerial_NextValue verifies [AC-70-2].
func TestDNSZone_SOASerial_NextValue(t *testing.T) {
	// [AC-70-2]
	ctx := context.Background()
	for _, c := range []struct {
		name, cur, want string
	}{
		{"older date", "2024010101", "2026102100"},
		{"today, second change", "2026102100", "2026102101"},
		{"today, 99 used up", "2026102199", "2026102200"},
		{"not date-shaped", "1", "2026102100"},
		{"not date-shaped, large", "3000000000", "3000000001"},
		{"future date", "2027010100", "2027010101"},
		{"at the limit", "4294967294", "4294967295"},
	} {
		t.Run(c.name, func(t *testing.T) {
			zone := filepath.Join(t.TempDir(), "example.com.zone")
			writeZoneFile(t, zone, soaZone(c.cur))
			p := dnszone.New(zone, dnszone.WithClock(soaClock))
			if err := p.Apply(ctx, soaUpdate("192.0.2.50")); err != nil {
				t.Fatalf("[AC-70-2] Apply: %v", err)
			}
			if got := soaSerialOf(t, readZoneFile(t, zone)); got != c.want {
				t.Errorf("[AC-70-2] serial %s → %s, want %s", c.cur, got, c.want)
			}
		})
	}

	// 同じ日に 2 回変えると nn が進む。
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, soaZone("2024010101"))
	p := dnszone.New(zone, dnszone.WithClock(soaClock))
	for i, v := range []string{"192.0.2.50", "192.0.2.51"} {
		if err := p.Apply(ctx, soaUpdate(v)); err != nil {
			t.Fatal(err)
		}
		if got, want := soaSerialOf(t, readZoneFile(t, zone)), "202610210"+strconv.Itoa(i); got != want {
			t.Errorf("[AC-70-2] change %d on the same day: serial %s, want %s", i+1, got, want)
		}
	}

	// 上限を超えるとき：上げずに PermanentError で止まり、ファイルは変えない。
	writeZoneFile(t, zone, soaZone("4294967295"))
	before := readZoneFile(t, zone)
	err := p.Apply(ctx, soaUpdate("192.0.2.99"))
	var pe *provider.PermanentError
	if err == nil || !errors.As(err, &pe) {
		t.Fatalf("[AC-70-2] Apply with serial 4294967295 = %v, want a PermanentError", err)
	}
	if got := readZoneFile(t, zone); got != before {
		t.Errorf("[AC-70-2] the zone changed although the Apply stopped:\n%s", got)
	}
}

// TestDNSZone_SOASerial_KeepAndNoSOA verifies [AC-70-3].
func TestDNSZone_SOASerial_KeepAndNoSOA(t *testing.T) {
	// [AC-70-3]
	ctx := context.Background()
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, soaZone("2024010101"))
	p := dnszone.New(zone, dnszone.WithClock(soaClock), dnszone.WithSOASerial(dnszone.SOASerialKeep))
	if err := p.Apply(ctx, soaUpdate("192.0.2.50")); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(soaZone("2024010101"), "192.0.2.1", "192.0.2.50", 1)
	if got := readZoneFile(t, zone); got != want {
		t.Errorf("[AC-70-3] keep: zone after Apply:\n%s\nwant:\n%s", got, want)
	}

	noSOA := "www.example.com. 300 IN A 192.0.2.1\n"
	writeZoneFile(t, zone, noSOA)
	p = dnszone.New(zone, dnszone.WithClock(soaClock))
	if err := p.Apply(ctx, soaUpdate("192.0.2.50")); err != nil {
		t.Fatalf("[AC-70-3] Apply on a zone without SOA: %v", err)
	}
	if got := readZoneFile(t, zone); got != "www.example.com. 300 IN A 192.0.2.50\n" {
		t.Errorf("[AC-70-3] zone without SOA after Apply:\n%s", got)
	}

	for _, v := range []string{"", "bump", "keep"} {
		if _, err := dnszone.ParseSOASerialMode(v); err != nil {
			t.Errorf("[AC-70-3] ParseSOASerialMode(%q): %v", v, err)
		}
	}
	if _, err := dnszone.ParseSOASerialMode("Bump"); err == nil {
		t.Errorf("[AC-70-3] ParseSOASerialMode(\"Bump\") accepted")
	}
}

// TestDNSZone_SOASerial_CLI verifies [AC-70-4] through the real binary.
func TestDNSZone_SOASerial_CLI(t *testing.T) {
	// [AC-70-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zone := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zone, soaZone("2024010101"))
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.50", "300", ""),
	})

	dayBefore := time.Now().UTC()
	out, errOut, code := runReconcile(t, intentDir, zone)
	if code != 0 {
		t.Fatalf("[AC-70-4] reconcile exited %d: %s %s", code, out, errOut)
	}
	dayAfter := time.Now().UTC()
	got := readZoneFile(t, zone)
	serial, _ := strconv.ParseUint(soaSerialOf(t, got), 10, 64)
	lo := uint64(dayBefore.Year())*1000000 + uint64(dayBefore.Month())*10000 + uint64(dayBefore.Day())*100
	hi := uint64(dayAfter.Year())*1000000 + uint64(dayAfter.Month())*10000 + uint64(dayAfter.Day())*100
	if serial != lo && serial != hi {
		t.Errorf("[AC-70-4] serial after reconcile = %d, want today's YYYYMMDD00 (%d)", serial, lo)
	}
	if want := strings.NewReplacer("2024010101", strconv.FormatUint(serial, 10), "192.0.2.1", "192.0.2.50").Replace(soaZone("2024010101")); got != want {
		t.Errorf("[AC-70-4] zone after reconcile:\n%s\nwant:\n%s", got, want)
	}

	// 次の plan は Converged（自分で上げたシリアルをずれと見なさない）、次の reconcile も上げない。
	if out, errOut, code := runPlan(t, intentDir, zone); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Errorf("[AC-70-4] plan after reconcile (exit %d) is not Converged:\n%s%s", code, out, errOut)
	}
	if out, errOut, code := runReconcile(t, intentDir, zone); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Errorf("[AC-70-4] 2nd reconcile (exit %d) is not Converged:\n%s%s", code, out, errOut)
	}
	if again := readZoneFile(t, zone); again != got {
		t.Errorf("[AC-70-4] a reconcile without changes changed the zone:\n%s", again)
	}

	// --zone-soa-serial keep：レコードは書くがシリアルは変えない。
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.51", "300", ""),
	})
	if out, errOut, code := runReconcile(t, intentDir, zone, "--zone-soa-serial", "keep"); code != 0 {
		t.Fatalf("[AC-70-4] reconcile --zone-soa-serial keep exited %d: %s %s", code, out, errOut)
	}
	kept := readZoneFile(t, zone)
	if want := strings.Replace(got, "192.0.2.50", "192.0.2.51", 1); kept != want {
		t.Errorf("[AC-70-4] keep: zone after reconcile:\n%s\nwant:\n%s", kept, want)
	}

	// 不正な値は起動時にエラー（--zone を受け付けるすべてのコマンド）。
	for _, args := range [][]string{
		{"plan", "--intent", intentDir, "--zone", zone},
		{"reconcile", "--intent", intentDir, "--zone", zone},
		{"status", "--intent", intentDir, "--zone", zone},
		{"import", "--provider", "dnszone", "--zone", zone, "--out", filepath.Join(tmp, "out")},
		{"sync", "--intent", intentDir, "--provider", "dnszone", "--zone", zone},
		{"--intent", intentDir, "--zone", zone, "--interval", "1h"}, // デーモン（#67）
	} {
		out, code := runSakutei(t, append(args, "--zone-soa-serial", "never")...)
		if code == 0 || !strings.Contains(out, "--zone-soa-serial") {
			t.Errorf("[AC-70-4] %v --zone-soa-serial never: exit %d, output:\n%s", args, code, out)
		}
	}
	if after := readZoneFile(t, zone); after != kept {
		t.Errorf("[AC-70-4] a command rejected at startup changed the zone:\n%s", after)
	}
}
