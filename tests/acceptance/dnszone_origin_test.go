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

// Backlog #69（§28.4 identity）: dnszone は $ORIGIN と "@" を解釈し、名前を FQDN
// にそろえて比べる。ID は "<FQDN>/<TYPE>"。書き戻しは元の書き方を保つ。
//
//	[AC-69-1]  $ORIGIN example.com. のゾーンで、相対名 www と絶対名 www.example.com.
//	           は同じレコード、絶対名 www. は別のレコード。"@" と途中の $ORIGIN も解決する
//	[AC-69-2]  書き戻しで元の書き方が保たれる（Update は相対名の行をその場で相対名の
//	           まま書き換える。同じレコードの 2 行目は消え、www. には触れない）
//	[AC-69-3]  intent の ID・name は FQDN。相対名の ID はロードで拒否され、ゾーンは
//	           変わらない。FQDN の intent は相対名の行と収束する（重複 CREATE なし）
//	[AC-69-4]  $ORIGIN の前の相対名はゾーン全体の parse error（unknown ≠ absent）
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/pkg/core"
)

const originZone = "$ORIGIN example.com.\n" +
	"@\t300 IN SOA ns1 admin 2026102101 3600 900 604800 300\n" +
	"@\t300 IN NS ns1\n" +
	"www 300 IN A 192.0.2.1\n" +
	"www. 300 IN A 198.51.100.1\n" +
	"www.example.com. 300 IN A 192.0.2.2\n" +
	"@ 300 IN TXT \"apex\"\n" +
	"$ORIGIN sub\n" +
	"host 300 IN A 192.0.2.3\n"

func listLocals(t *testing.T, p *dnszone.Provider) []string {
	t.Helper()
	res, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []string
	for _, id := range res.IDs {
		out = append(out, id.Local)
	}
	sort.Strings(out)
	return out
}

func readValue(t *testing.T, p *dnszone.Provider, local string) string {
	t.Helper()
	obs, err := p.Read(context.Background(), local)
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read(%s) = %+v, %v", local, obs, err)
	}
	if obs.State == nil {
		return "<absent>"
	}
	return obs.State["value"].Value.(string)
}

// TestDNSZone_OriginResolvesNames verifies [AC-69-1].
func TestDNSZone_OriginResolvesNames(t *testing.T) {
	// [AC-69-1]
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, originZone)
	p := dnszone.New(zone)

	want := []string{"example.com./TXT", "host.sub.example.com./A", "www./A", "www.example.com./A"}
	if got := listLocals(t, p); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("[AC-69-1] List = %v, want %v (www and www.example.com. are one id; www. is another)", got, want)
	}
	// 相対名 www と絶対名 www.example.com. は同じレコード：先に書いた行の値が見える。
	if v := readValue(t, p, "www.example.com./A"); v != "192.0.2.1" {
		t.Errorf("[AC-69-1] Read(www.example.com./A) value = %q, want the relative line's 192.0.2.1", v)
	}
	// 絶対名 www. は別のレコード。
	if v := readValue(t, p, "www./A"); v != "198.51.100.1" {
		t.Errorf("[AC-69-1] Read(www./A) value = %q, want 198.51.100.1", v)
	}
	obs, _ := p.Read(context.Background(), "www.example.com./A")
	if n := obs.State["name"].Value; n != "www.example.com." {
		t.Errorf("[AC-69-1] observed name = %v, want the FQDN www.example.com.", n)
	}
	if v := readValue(t, p, "host.sub.example.com./A"); v != "192.0.2.3" {
		t.Errorf("[AC-69-1] relative $ORIGIN sub: Read(host.sub.example.com./A) = %q, want 192.0.2.3", v)
	}
	// BulkObserve は List と同じ ID で、Read と同じ値を返す。
	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := b.Observations[core.NewID(dnszone.ProviderName, core.NewScope(nil), "www.example.com./A").Key()]
	if v := got.State["value"].Value; v != "192.0.2.1" {
		t.Errorf("[AC-69-1] BulkObserve www.example.com./A = %v, want 192.0.2.1", v)
	}
}

// TestDNSZone_OriginWriteBackKeepsForm verifies [AC-69-2].
func TestDNSZone_OriginWriteBackKeepsForm(t *testing.T) {
	// [AC-69-2]
	ctx := context.Background()
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, originZone)
	// 行の書き方だけを見るので、SOA のシリアルは上げない（#70 は dnszone_soa_serial_test.go）。
	p := dnszone.New(zone, dnszone.WithSOASerial(dnszone.SOASerialKeep))
	id := func(local string) core.ID { return core.NewID(dnszone.ProviderName, core.NewScope(nil), local) }

	err := p.Apply(ctx, core.Plan{Ops: []core.Op{
		{Kind: core.OpKindUpdate, Target: id("www.example.com./A"), Fields: []core.FieldDiff{{Field: "value", From: "192.0.2.1", To: "192.0.2.50"}}},
		{Kind: core.OpKindUpdate, Target: id("example.com./TXT"), Fields: []core.FieldDiff{{Field: "value", From: `"apex"`, To: `"apex2"`}}},
		{Kind: core.OpKindUpdate, Target: id("host.sub.example.com./A"), Fields: []core.FieldDiff{{Field: "value", From: "192.0.2.3", To: "192.0.2.33"}}},
		{Kind: core.OpKindCreate, Target: id("new.example.com./A"), Fields: []core.FieldDiff{{Field: "value", To: "192.0.2.4"}, {Field: "ttl", To: "300"}}},
	}})
	if err != nil {
		t.Fatalf("[AC-69-2] Apply: %v", err)
	}
	want := "$ORIGIN example.com.\n" +
		"@\t300 IN SOA ns1 admin 2026102101 3600 900 604800 300\n" +
		"@\t300 IN NS ns1\n" +
		"www 300 IN A 192.0.2.50\n" +
		"www. 300 IN A 198.51.100.1\n" +
		"@ 300 IN TXT \"apex2\"\n" +
		"$ORIGIN sub\n" +
		"host 300 IN A 192.0.2.33\n" +
		"new.example.com. 300 IN A 192.0.2.4\n"
	if got := readZoneFile(t, zone); got != want {
		t.Errorf("[AC-69-2] zone after Apply:\n%s\nwant:\n%s", got, want)
	}

	// Delete は書き方によらず同じレコードの行を消し、www. には触れない。
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id("www.example.com./A")}}}); err != nil {
		t.Fatalf("[AC-69-2] Delete: %v", err)
	}
	got := readZoneFile(t, zone)
	if strings.Contains(got, "www 300") || !strings.Contains(got, "www. 300 IN A 198.51.100.1") {
		t.Errorf("[AC-69-2] Delete www.example.com./A must remove the relative line and keep www.:\n%s", got)
	}
}

// TestDNSZone_OriginIntentIsFQDN verifies [AC-69-3].
func TestDNSZone_OriginIntentIsFQDN(t *testing.T) {
	// [AC-69-3]
	p := dnszone.New("/dev/null")
	for _, c := range []struct {
		local  string
		fields map[string]string
		ok     bool
	}{
		{"www.example.com./A", map[string]string{"name": "www.example.com.", "type": "A"}, true},
		// 大文字・小文字は List の形（名前は小文字、型は大文字）でなければ拒否（#71）。
		{"WWW.example.com./A", map[string]string{"name": "www.example.com.", "type": "A"}, false},
		{"www.example.com./a", nil, false},
		{"www.example.com./A", map[string]string{"name": "WWW.example.com."}, false},
		{"www.example.com./A", map[string]string{"type": "a"}, false},
		{"www/A", map[string]string{"name": "www", "type": "A"}, false},
		{"@/A", nil, false},
		{"www.example.com./A", map[string]string{"name": "www"}, false},
		{"www.example.com./A", map[string]string{"name": "www."}, false},
		{"www.example.com./A", map[string]string{"type": "AAAA"}, false},
	} {
		if err := p.ValidateIntent(c.local, c.fields); (err == nil) != c.ok {
			t.Errorf("[AC-69-3] ValidateIntent(%q, %v) = %v, want ok=%v", c.local, c.fields, err, c.ok)
		}
	}

	tmp := t.TempDir()
	zone := filepath.Join(tmp, "example.com.zone")
	const initial = "$ORIGIN example.com.\nwww 300 IN A 192.0.2.1\n"

	// 相対名の ID はロードで拒否。ゾーンは書き換わらない。
	bad := filepath.Join(tmp, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	writeZoneFile(t, zone, initial)
	writeDNSZoneIntent(t, bad, []map[string]any{
		dnszoneResource("www/A", "www", "A", "192.0.2.9", "300", "PruneWithinScope"),
	})
	stdout, stderr, code := runReconcile(t, bad, zone)
	if code == 0 || !strings.Contains(stderr, "not a fully-qualified name") {
		t.Errorf("[AC-69-3] reconcile with id www/A must fail at load; exit %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got := readZoneFile(t, zone); got != initial {
		t.Errorf("[AC-69-3] rejected intent must not touch the zone:\n%s", got)
	}

	// FQDN の intent は相対名の行と同じレコード：値が同じなら収束、違えば その場で UPDATE。
	good := filepath.Join(tmp, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDNSZoneIntent(t, good, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.1", "300", "PruneWithinScope"),
	})
	stdout, stderr, code = runPlan(t, good, zone)
	if code != 0 || !strings.Contains(stdout, "Converged (no changes)") {
		t.Errorf("[AC-69-3] FQDN intent over a relative line must be converged; exit %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	writeDNSZoneIntent(t, good, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.7", "300", "PruneWithinScope"),
	})
	if stdout, stderr, code = runReconcile(t, good, zone); code != 0 {
		t.Fatalf("[AC-69-3] reconcile: exit %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got, want := readZoneFile(t, zone), "$ORIGIN example.com.\nwww 300 IN A 192.0.2.7\n"; got != want {
		t.Errorf("[AC-69-3] zone after reconcile = %q, want %q (one record, relative form kept)", got, want)
	}

	// import が出す ID も FQDN で、そのまま plan が収束する。
	out := filepath.Join(tmp, "imported")
	if o, code := runSakutei(t, "import", "--provider", "dnszone", "--zone", zone, "--out", out, "--samples", "1"); code != 0 {
		t.Fatalf("[AC-69-3] import: exit %d\n%s", code, o)
	}
	if y := readFile(t, filepath.Join(out, "imported-dnszone.yaml")); !strings.Contains(y, "www.example.com./A") {
		t.Errorf("[AC-69-3] import must emit the FQDN id www.example.com./A:\n%s", y)
	}
	assertPlanConverged(t, "[AC-69-3]", "--intent", out, "--zone", zone)
}

// TestDNSZone_RelativeNameWithoutOrigin verifies [AC-69-4].
func TestDNSZone_RelativeNameWithoutOrigin(t *testing.T) {
	// [AC-69-4]
	for name, content := range map[string]string{
		"relative": "www 300 IN A 192.0.2.1\n$ORIGIN example.com.\n",
		"@":        "@ 300 IN A 192.0.2.1\n",
	} {
		t.Run(name, func(t *testing.T) {
			zone := filepath.Join(t.TempDir(), "example.com.zone")
			writeZoneFile(t, zone, content)
			p := dnszone.New(zone)
			if _, err := p.List(context.Background(), core.NewScope(nil)); err == nil || !strings.Contains(err.Error(), "no $ORIGIN") {
				t.Errorf("[AC-69-4] List must fail on a relative owner with no $ORIGIN, got %v", err)
			}
			if obs, _ := p.Read(context.Background(), "www.example.com./A"); obs.Availability != core.AvailabilityUnknown {
				t.Errorf("[AC-69-4] Read must be Unknown, got %v", obs.Availability)
			}
		})
	}
}
