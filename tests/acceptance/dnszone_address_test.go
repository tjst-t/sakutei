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

// dnszone_address_test.go は Backlog #98（採番の試用、docs/trial/allocation）で見つけた
// 不具合の受け入れテスト。
//
// v1 のプールは整数だけなので、A レコードの value を from_pool にすると "A 8001" のような
// 行がゾーンに書かれていた。dnszone 自身は読み戻せるが、DNS サーバはゾーン全体の読み込みを
// 拒む（ゾーンのほかのレコードも答えなくなる）。A に IPv4 以外、AAAA に IPv6 以外の value は
// 書かずに、そのリソースだけ Permanent で止める（#37 の「読み戻せない行は書かない」と同じ形）。
//
// AC tags:
//
//	[AC-98-1]  Apply は A に IPv4 以外・AAAA に IPv6 以外の value を書かずに Permanent で失敗し、
//	           ゾーンは書き換わらない。正しいアドレスは従来どおり書く。intent に直に書いた
//	           誤ったアドレスも同じ（ロードでは拒否しない。ロード時の検証は割当のあとの値にも
//	           かかり、そこで落とすと周期全体が止まるため、このリソースだけを止める）
//	[AC-98-2]  from_pool で整数が A に入っても、reconcile はそのリソースだけを止め、
//	           ほかのレコードは収束する
package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

func dnsCreate(id, value string) core.Plan {
	return core.Plan{Ops: []core.Op{{
		Kind:   core.OpKindCreate,
		Target: core.NewID(dnszone.ProviderName, core.NewScope(nil), id),
		Fields: []core.FieldDiff{{Field: "value", To: value}, {Field: "ttl", To: "300"}},
	}}}
}

func TestDNSZone_Address_ApplyRefusesNonAddress(t *testing.T) {
	// [AC-98-1]
	bad := map[string]string{
		"a.example.com./A":    "8001",
		"b.example.com./A":    "2001:db8::1",
		"c.example.com./A":    "::ffff:192.0.2.1",
		"d.example.com./AAAA": "192.0.2.1",
		"e.example.com./AAAA": "fe80::1%eth0",
		"f.example.com./AAAA": "8001",
	}
	for id, value := range bad {
		t.Run(id, func(t *testing.T) {
			zonePath := filepath.Join(t.TempDir(), "example.com.zone")
			const zone = "$ORIGIN example.com.\nkeep.example.com. 300 IN A 192.0.2.1\n"
			writeZoneFile(t, zonePath, zone)
			err := dnszone.New(zonePath).Apply(context.Background(), dnsCreate(id, value))
			var perm *provider.PermanentError
			if err == nil || !errors.As(err, &perm) {
				t.Fatalf("[AC-98-1] Apply(%s = %q) must fail with a PermanentError, got %v", id, value, err)
			}
			if got := readFile(t, zonePath); got != zone {
				t.Fatalf("[AC-98-1] the zone must not be rewritten:\n%s", got)
			}
		})
	}
	good := map[string]string{
		"a.example.com./A":    "192.0.2.10",
		"b.example.com./AAAA": "2001:db8::10",
		"c.example.com./AAAA": "::ffff:192.0.2.1",
		"d.example.com./TXT":  "8001",
	}
	for id, value := range good {
		zonePath := filepath.Join(t.TempDir(), "example.com.zone")
		writeZoneFile(t, zonePath, "$ORIGIN example.com.\n")
		if err := dnszone.New(zonePath).Apply(context.Background(), dnsCreate(id, value)); err != nil {
			t.Fatalf("[AC-98-1] Apply(%s = %q) must succeed: %v", id, value, err)
		}
		if got := readFile(t, zonePath); !strings.Contains(got, " "+value+"\n") {
			t.Fatalf("[AC-98-1] %s = %q must be written:\n%s", id, value, got)
		}
	}
}

func TestDNSZone_Address_FromPoolIntoA(t *testing.T) {
	// [AC-98-2]
	tmp := t.TempDir()
	intent := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "main.yaml"), `version: "1"
pools:
  - name: ports
    range: "8000-8009"
resources:
  - provider: dnszone
    id: port.example.com./TXT
    fields:
      name: port.example.com.
      type: TXT
      value:
        from_pool: ports
  - provider: dnszone
    id: app.example.com./A
    fields:
      name: app.example.com.
      type: A
      value:
        from_pool: ports
`)
	zonePath := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zonePath, "$ORIGIN example.com.\n")
	out, _ := runSakutei(t, "reconcile", "--intent", intent, "--zone", zonePath, "--alloc-store", filepath.Join(tmp, "alloc.json"))
	zone := readFile(t, zonePath)
	if !strings.Contains(zone, "port.example.com. 300 IN TXT 8000") {
		t.Fatalf("[AC-98-2] the TXT record must converge:\n%s\n%s", zone, out)
	}
	if strings.Contains(zone, "app.example.com.") {
		t.Fatalf("[AC-98-2] an integer must not be written into an A record:\n%s", zone)
	}
	if !strings.Contains(out, "not an IPv4 address") {
		t.Fatalf("[AC-98-2] reconcile must say why app.example.com./A was not written:\n%s", out)
	}
}
