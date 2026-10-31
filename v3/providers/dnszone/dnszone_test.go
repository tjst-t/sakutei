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

package dnszone_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/dnszone"
)

const trialZone = `$ORIGIN example.com.
@	300 IN SOA ns1 admin 2026102101 3600 900 604800 300
@	300 IN NS ns1
www	300 IN A 192.0.2.10
`

var zoneRange = []core.Range{{Provider: "dns", Args: map[string]string{"zone": "example.com."}}}

func provider(t *testing.T, zone string) (*dnszone.Provider, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "example.com.zone")
	if err := os.WriteFile(path, []byte(zone), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := dnszone.Module(map[string]any{"zone_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	dp := p.(*dnszone.Provider)
	dp.Now = func() time.Time { return time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC) }
	if d := dp.Validate(nil, zoneRange); len(d) > 0 {
		t.Fatal(d)
	}
	return dp, path
}

func rec(name string) core.ID { return core.ID{Type: "dns/record", Name: name} }

func observe(t *testing.T, p core.Provider, id core.ID) core.Observation {
	t.Helper()
	o, err := p.Observe(context.Background(), []core.ID{id})
	if err != nil {
		t.Fatal(err)
	}
	return o[id]
}

// $ORIGIN・@・相対名を FQDN にして観測する（E-I7）。
func TestObserve(t *testing.T) {
	p, _ := provider(t, trialZone)
	if o := observe(t, p, rec("www.example.com./A")); o.Availability != core.Present || o.Fields["value"] != "192.0.2.10" || o.Fields["ttl"] != int64(300) {
		t.Fatalf("%+v", o)
	}
	if o := observe(t, p, rec("example.com./NS")); o.Fields["value"] != "ns1.example.com." {
		t.Fatalf("%+v", o)
	}
	if o := observe(t, p, rec("api.example.com./A")); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
	if o := observe(t, p, rec("www.other.org./A")); o.Availability != core.Unknown {
		t.Fatalf("範囲の外: %+v", o)
	}
	l, err := p.List(context.Background(), zoneRange[0])
	if err != nil || !l.Complete || len(l.IDs) != 2 {
		t.Fatalf("%+v %v", l, err)
	}
}

// 書き換えた Apply・Delete のときだけ SOA のシリアルを上げる（E-B1）。触らない行は残す。
func TestApplyBumpsSerial(t *testing.T) {
	p, path := provider(t, trialZone)
	ctx := context.Background()
	www := rec("www.example.com./A")
	if err := p.Apply(ctx, core.WriteContext{}, www, core.Fields{"value": "192.0.2.20"}, observe(t, p, www)); err != nil {
		t.Fatal(err)
	}
	if s, _ := dnszone.ParseSerial(path, "example.com."); s != 2026110500 {
		t.Fatalf("シリアル %d", s)
	}
	if o := observe(t, p, www); o.Fields["value"] != "192.0.2.20" || o.Fields["ttl"] != int64(300) {
		t.Fatalf("%+v", o)
	}
	api := rec("api.example.com./A")
	if err := p.Apply(ctx, core.WriteContext{}, api, core.Fields{"values": []any{"192.0.2.31", "192.0.2.30"}, "ttl": int64(60)}, observe(t, p, api)); err != nil {
		t.Fatal(err)
	}
	if s, _ := dnszone.ParseSerial(path, "example.com."); s != 2026110501 {
		t.Fatalf("シリアル %d", s)
	}
	if o := observe(t, p, api); len(o.Fields["values"].([]any)) != 2 || o.Fields["ttl"] != int64(60) {
		t.Fatalf("%+v", o)
	}
	if err := p.Delete(ctx, core.WriteContext{}, api, observe(t, p, api)); err != nil {
		t.Fatal(err)
	}
	if o := observe(t, p, api); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "@\t300 IN NS ns1") || !strings.HasPrefix(string(data), "$ORIGIN example.com.\n") {
		t.Fatalf("触らない行が変わった:\n%s", data)
	}
}

// 括弧の複数行の SOA と、owner を省いた行（消した行の owner を書き足す）。
func TestMultiLineAndBlankOwner(t *testing.T) {
	p, path := provider(t, `$ORIGIN example.com.
$TTL 1h
@ IN SOA ns1.example.com. admin.example.com. (
        42 ; serial
        3600 900 604800 300 )
www IN A 192.0.2.10
    IN TXT "hello world"
`)
	www := rec("www.example.com./A")
	txt := rec("www.example.com./TXT")
	if o := observe(t, p, txt); o.Fields["value"] != `"hello world"` || o.Fields["ttl"] != int64(3600) {
		t.Fatalf("%+v", o)
	}
	if err := p.Delete(context.Background(), core.WriteContext{}, www, observe(t, p, www)); err != nil {
		t.Fatal(err)
	}
	if s, err := dnszone.ParseSerial(path, "example.com."); s != 43 || err != nil {
		t.Fatalf("シリアル %d %v", s, err)
	}
	if o := observe(t, p, txt); o.Availability != core.Present {
		t.Fatalf("TXT の owner が失われた: %+v", o)
	}
}

// ゾーンファイルが無い・壊れていれば Unknown（無いと読まない、不変条件 3）。
func TestUnknown(t *testing.T) {
	p, path := provider(t, "www IN A (\n")
	if o := observe(t, p, rec("www.example.com./A")); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
	os.Remove(path)
	if o := observe(t, p, rec("www.example.com./A")); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
	if _, err := p.List(context.Background(), zoneRange[0]); err == nil {
		t.Fatal("List が誤りを返さない")
	}
}

// 不変条件 25・E-I7：相対名・SOA・正規形でない値・範囲の外・重なるゾーンは断る。
func TestValidateRejects(t *testing.T) {
	p, _ := provider(t, trialZone)
	cases := []struct {
		r    core.Resource
		want string
	}{
		{core.Resource{ID: rec("www/A")}, "FQDN"},
		{core.Resource{ID: rec("Www.example.com./A")}, "FQDN"},
		{core.Resource{ID: rec("example.com./SOA")}, "SOA"},
		{core.Resource{ID: rec("www.example.com./HINFO")}, "扱わない"},
		{core.Resource{ID: rec("x.other.org./A")}, "範囲"},
		{core.Resource{ID: rec("c.example.com./CNAME"), Fields: core.Fields{"value": "www"}}, `正規形 "www.example.com."`},
		{core.Resource{ID: rec("c.example.com./A"), Fields: core.Fields{"value": "1.1.1.1", "values": []any{"1.1.1.1"}}}, "一緒に"},
	}
	for _, c := range cases {
		d := p.Validate([]core.Resource{c.r}, zoneRange)
		if len(d) == 0 || !strings.Contains(d[0].Message, c.want) {
			t.Errorf("%s: %v", c.r.ID, d)
		}
	}
	d := p.Validate(nil, append(zoneRange, core.Range{Provider: "dns", Args: map[string]string{"zone": "sub.example.com."}}))
	if len(d) == 0 || !strings.Contains(d[0].Message, "重なる") {
		t.Errorf("重なり: %v", d)
	}
}
