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

// Backlog #71（§28.4 identity）: dnszone の ID は小文字の FQDN（型は大文字）に
// そろえる。ゾーンファイルの書き方（大文字・小文字）は書き戻しで保つ。
//
//	[AC-71-1]  ゾーンに大文字を含む名前（WWW）があっても、List・BulkObserve の ID
//	           と Read の name は小文字
//	[AC-71-2]  intent の小文字 ID はその行と同じリソース：PruneWithinScope でも
//	           prune されず、収束し、揺れない。Update は行の書き方（WWW）を保つ
//	[AC-71-3]  大文字を含む intent の ID はロードで拒否され（エラーに小文字の形）、
//	           ゾーンは変わらない
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/pkg/core"
)

const caseZone = "$ORIGIN example.com.\n" +
	"WWW 300 IN A 192.0.2.1\n" +
	"Mail.Example.COM. 300 IN MX 10 mx.example.com.\n"

// TestDNSZone_IDIsLowerCase verifies [AC-71-1].
func TestDNSZone_IDIsLowerCase(t *testing.T) {
	// [AC-71-1]
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, caseZone)
	p := dnszone.New(zone)

	want := []string{"mail.example.com./MX", "www.example.com./A"}
	if got := listLocals(t, p); !reflect.DeepEqual(got, want) {
		t.Errorf("[AC-71-1] List = %v, want %v", got, want)
	}
	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Observations) != len(want) {
		t.Errorf("[AC-71-1] BulkObserve has %d entries, want %d", len(b.Observations), len(want))
	}
	for _, local := range want {
		o, ok := b.Observations[core.NewID(dnszone.ProviderName, core.NewScope(nil), local).Key()]
		if !ok {
			t.Errorf("[AC-71-1] BulkObserve has no entry for %s", local)
			continue
		}
		name := strings.TrimSuffix(local, "/A")
		name = strings.TrimSuffix(name, "/MX")
		if got := o.State["name"].Value; got != name {
			t.Errorf("[AC-71-1] BulkObserve %s name = %v, want %s", local, got, name)
		}
		obs, err := p.Read(context.Background(), local)
		if err != nil || obs.State == nil {
			t.Fatalf("[AC-71-1] Read(%s) = %+v, %v", local, obs, err)
		}
		if !reflect.DeepEqual(obs.State, o.State) {
			t.Errorf("[AC-71-1] Read(%s) = %v, BulkObserve = %v", local, obs.State, o.State)
		}
	}
}

// TestDNSZone_LowerCaseIntentNotPruned verifies [AC-71-2].
func TestDNSZone_LowerCaseIntentNotPruned(t *testing.T) {
	// [AC-71-2]
	tmp := t.TempDir()
	zone := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zone, caseZone)
	intent := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDNSZoneIntent(t, intent, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.1", "300", "PruneWithinScope"),
		dnszoneResource("mail.example.com./MX", "mail.example.com.", "MX", "10 mx.example.com.", "300", "PruneWithinScope"),
	})

	stdout, stderr, code := runPlan(t, intent, zone)
	if code != 0 || !strings.Contains(stdout, "Converged (no changes)") {
		t.Errorf("[AC-71-2] lower-case intent over WWW must be converged; exit %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	// 2 周期まわしても prune・CREATE で揺れない。
	for i := 0; i < 2; i++ {
		if stdout, stderr, code = runReconcile(t, intent, zone); code != 0 {
			t.Fatalf("[AC-71-2] reconcile %d: exit %d\nstdout=%s\nstderr=%s", i, code, stdout, stderr)
		}
		if got := readZoneFile(t, zone); got != caseZone {
			t.Errorf("[AC-71-2] reconcile %d must not touch the zone:\n%s", i, got)
		}
	}

	// 値を変えると、その行を書き方（WWW）のまま書き換える。
	writeDNSZoneIntent(t, intent, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.7", "300", "PruneWithinScope"),
		dnszoneResource("mail.example.com./MX", "mail.example.com.", "MX", "10 mx.example.com.", "300", "PruneWithinScope"),
	})
	if stdout, stderr, code = runReconcile(t, intent, zone); code != 0 {
		t.Fatalf("[AC-71-2] reconcile: exit %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	want := "$ORIGIN example.com.\n" +
		"WWW 300 IN A 192.0.2.7\n" +
		"Mail.Example.COM. 300 IN MX 10 mx.example.com.\n"
	if got := readZoneFile(t, zone); got != want {
		t.Errorf("[AC-71-2] zone after reconcile = %q, want %q (written as it was)", got, want)
	}
}

// TestDNSZone_UpperCaseIntentRejected verifies [AC-71-3].
func TestDNSZone_UpperCaseIntentRejected(t *testing.T) {
	// [AC-71-3]
	tmp := t.TempDir()
	zone := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zone, caseZone)
	intent := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDNSZoneIntent(t, intent, []map[string]any{
		dnszoneResource("WWW.example.com./A", "www.example.com.", "A", "192.0.2.1", "300", "PruneWithinScope"),
	})
	stdout, stderr, code := runReconcile(t, intent, zone)
	if code == 0 || !strings.Contains(stderr, `"www.example.com./A"`) {
		t.Errorf("[AC-71-3] reconcile with id WWW.example.com./A must fail at load naming the lower-case id; exit %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got := readZoneFile(t, zone); got != caseZone {
		t.Errorf("[AC-71-3] rejected intent must not touch the zone:\n%s", got)
	}
}
