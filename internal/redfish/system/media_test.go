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

package system

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/redfish"
	"github.com/tjst-t/sakutei/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

func TestMedia_ValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(NewMedia(nil)); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	caps := NewMedia(nil).Capabilities()
	if caps.List || caps.BulkObserve || caps.Pruneable || !caps.TrueObserve {
		t.Errorf("Capabilities = %+v, want the standard tier (no List / BulkObserve, not pruneable)", caps)
	}
}

// 契約テスト（#32）を偽 BMC の上で回す。CD のドライブは前からあるので Preexisting。
func TestMedia_Contract(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	ct := sdk.ContractTester{
		Provider:      NewMedia(map[string]redfish.Client{"node1": c}),
		Scope:         core.NewScope(nil),
		SampleID:      MediaID("node1"),
		Preexisting:   true,
		SampleDesired: map[string]string{FieldName: "node1", FieldImage: "https://repo.example/a.iso"},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
}

func TestMedia_ReadNotInsertedIsEmpty(t *testing.T) {
	clk := newClock()
	srv, c := newBMC(t, redfishtest.Options{Now: clk.Now})
	p := NewMedia(map[string]redfish.Client{"n": c})
	srv.SetMediaFetchFails(true)
	if err := c.InsertMedia(context.Background(), "/isos/a.iso"); err != nil {
		t.Fatal(err)
	}
	obs, err := p.Read(context.Background(), MediaID("n"))
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read = %+v, %v", obs, err)
	}
	// 取得に失敗して Inserted は false、Image は BMC に残っている。
	want := map[string]string{FieldName: "n", FieldImage: "", FieldInserted: "false", FieldMediaImage: "/isos/a.iso"}
	for k, v := range want {
		if got := sdkString(obs.State[k]); got != v {
			t.Errorf("State[%s] = %q, want %q", k, got, v)
		}
	}
	schema := NewMedia(nil).Schema()
	for _, k := range []string{FieldInserted, FieldMediaImage} {
		if s := schema.Fields[k]; !s.Computed || s.Identity {
			t.Errorf("Schema %s = %+v, want Computed and not Identity", k, s)
		}
	}
}

func TestMedia_ReadUnreachableIsUnknown(t *testing.T) {
	cases := map[string]func(*redfishtest.Server){
		"connection closed": func(s *redfishtest.Server) { s.SetUnreachable(true) },
		"5xx":               func(s *redfishtest.Server) { s.SetFailStatus(http.StatusServiceUnavailable) },
		"timeout":           func(s *redfishtest.Server) { s.SetDelay(2 * time.Second) },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			srv, c := newBMC(t, redfishtest.Options{})
			break_(srv)
			obs, err := NewMedia(map[string]redfish.Client{"n": c}).Read(context.Background(), MediaID("n"))
			if obs.Availability != core.AvailabilityUnknown || obs.State != nil || !sdk.IsTransient(err) {
				t.Fatalf("Read = %+v, %v; want Unknown with a transient error", obs, err)
			}
		})
	}
	for _, id := range []string{"nobody/cd", "n", "/cd"} {
		obs, err := NewMedia(nil).Read(context.Background(), id)
		if obs.Availability != core.AvailabilityUnknown || obs.State != nil || err == nil {
			t.Fatalf("Read(%q) = %+v, %v; want Unknown and an error", id, obs, err)
		}
	}
}

func TestMedia_ValidateIntent(t *testing.T) {
	p := NewMedia(map[string]redfish.Client{"n": nil})
	for _, f := range []map[string]string{
		{"name": "n", "image": "https://repo.example/a.iso"},
		{"image": "http://10.0.0.1/a.iso"},
		{"image": "nfs://10.0.0.1/isos/a.iso"},
		{"image": "/isos/a.iso"},
		{"image": ""},
		{},
	} {
		if err := p.ValidateIntent("n/cd", f); err != nil {
			t.Errorf("ValidateIntent(%v) = %v, want nil", f, err)
		}
	}
	bad := []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"n/cd", map[string]string{"image": "a.iso"}, "want a URL"},
		{"n/cd", map[string]string{"inserted": "true"}, "observed only"},
		{"n/cd", map[string]string{"media_image": "/a.iso"}, "observed only"},
		{"n/cd", map[string]string{"power": "On"}, "unknown field"},
		{"n/cd", map[string]string{"name": "m"}, "must equal the node name"},
		{"n", map[string]string{"image": "/a.iso"}, `want "<node>/cd"`},
		{"m/cd", map[string]string{"image": "/a.iso"}, "no BMC is configured"},
	}
	for _, b := range bad {
		err := p.ValidateIntent(b.id, b.fields)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("ValidateIntent(%q, %v) = %v, want an error containing %q", b.id, b.fields, err, b.want)
		}
	}
}

// mediaPlan は偽 BMC を Read して diff.Engine.Plan を通す。
func mediaPlan(t *testing.T, p *MediaProvider, node, image string) core.Plan {
	t.Helper()
	id := MediaID(node)
	obs, err := p.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{
		ID: core.NewID(MediaName, core.NewScope(nil), id), Fields: map[string]string{FieldName: node, FieldImage: image},
	}, cause.CauseContext{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return pl
}

func mediaWrites(srv *redfishtest.Server) []string {
	var out []string
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			switch {
			case strings.HasSuffix(r.Path, "InsertMedia"):
				out = append(out, "Insert "+r.Body)
			case strings.HasSuffix(r.Path, "EjectMedia"):
				out = append(out, "Eject")
			default:
				out = append(out, r.Method+" "+r.Path)
			}
		}
	}
	srv.ClearRequests()
	return out
}

func TestMedia_ApplyInsertSwapEject(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	p := NewMedia(map[string]redfish.Client{"n": c})
	steps := []struct {
		image string
		want  []string
	}{
		{"/isos/a.iso", []string{`Insert {"Image":"/isos/a.iso"}`}},
		{"/isos/a.iso", nil},
		{"https://repo.example/b.iso", []string{"Eject", `Insert {"Image":"https://repo.example/b.iso"}`}},
		{"", []string{"Eject"}},
		{"", nil},
	}
	for i, s := range steps {
		pl := mediaPlan(t, p, "n", s.image)
		for _, op := range pl.Ops {
			if op.Destructive {
				t.Fatalf("step %d: op %+v is destructive; media operations are not", i, op)
			}
		}
		if (len(pl.Ops) == 0) != (len(s.want) == 0) {
			t.Fatalf("step %d: plan = %+v, want writes %v", i, pl, s.want)
		}
		if err := p.Apply(context.Background(), pl); err != nil {
			t.Fatalf("step %d: Apply: %v", i, err)
		}
		if got := mediaWrites(srv); strings.Join(got, "|") != strings.Join(s.want, "|") {
			t.Fatalf("step %d (image %q): writes = %v, want %v", i, s.image, got, s.want)
		}
		in, img := srv.Media()
		if (s.image == "") == in || (in && img != s.image) {
			t.Fatalf("step %d: media = %v %q, want %q", i, in, img, s.image)
		}
	}
}

// 同じ plan を 2 回適用しても 2 回目は何も送らない（Apply は読み直す）。
func TestMedia_ApplyIsIdempotent(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	if err := c.InsertMedia(context.Background(), "/isos/a.iso"); err != nil {
		t.Fatal(err)
	}
	srv.ClearRequests()
	p := NewMedia(map[string]redfish.Client{"n": c})
	pl := mediaPlan(t, p, "n", "/isos/b.iso")
	for i := 0; i < 2; i++ {
		if err := p.Apply(context.Background(), pl); err != nil {
			t.Fatal(err)
		}
	}
	if got := mediaWrites(srv); len(got) != 2 {
		t.Fatalf("writes = %v, want one Eject and one Insert in total", got)
	}
}

// 取得に失敗して外れた（Inserted false・Image は残る）ら、Eject せずに入れ直す。
func TestMedia_ApplyReinsertsAfterFetchFailure(t *testing.T) {
	clk := newClock()
	srv, c := newBMC(t, redfishtest.Options{Now: clk.Now})
	p := NewMedia(map[string]redfish.Client{"n": c})
	srv.SetMediaFetchFails(true)
	if err := p.Apply(context.Background(), mediaPlan(t, p, "n", "/isos/a.iso")); err != nil {
		t.Fatal(err)
	}
	mediaWrites(srv)
	srv.SetMediaFetchFails(false)
	pl := mediaPlan(t, p, "n", "/isos/a.iso")
	if len(pl.Ops) != 1 {
		t.Fatalf("plan after the drop = %+v, want one op (a drift)", pl)
	}
	if err := p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if got := mediaWrites(srv); len(got) != 1 || !strings.HasPrefix(got[0], "Insert") {
		t.Fatalf("writes = %v, want one Insert", got)
	}
	if in, _ := srv.Media(); !in {
		t.Fatalf("media not inserted")
	}
}

func TestMedia_ApplyErrors(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	p := NewMedia(map[string]redfish.Client{"n": c})
	pl := mediaPlan(t, p, "n", "/isos/a.iso")
	srv.SetUnreachable(true)
	if err := p.Apply(context.Background(), pl); !sdk.IsTransient(err) {
		t.Fatalf("Apply while unreachable = %v, want a transient error", err)
	}
	srv.SetUnreachable(false)
	if got := mediaWrites(srv); len(got) != 0 {
		t.Fatalf("writes while unreachable = %v", got)
	}
	del := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: core.NewID(MediaName, core.NewScope(nil), MediaID("n"))}}}
	if err := p.Apply(context.Background(), del); err == nil || sdk.IsTransient(err) {
		t.Fatalf("Apply Delete = %v, want a permanent error", err)
	}
}
