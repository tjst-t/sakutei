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

// redfish_media_test.go — #82: Redfish の「仮想メディア」（CD）を偽 BMC を相手に
// デーモンの経路（RunCycle）で収束させる（§29.1 の（6））。
//
//	[AC-82-1]  image の宣言で Insert し、次の周は何も操作しない。
//	[AC-82-2]  違う image を宣言すると同じ周で Eject → Insert（差し替え）。
//	[AC-82-3]  空の image を宣言すると Eject。
//	[AC-82-4]  BMC の取得失敗で Inserted が false に戻れば、次の周に入れ直す。
//	[AC-82-5]  BMC に届かない周は Unknown で、何も操作しない。
//	[AC-82-6]  仮想メディアの操作は破壊的に数えない（予算 0 でも止まらない）。
//	[AC-82-7]  システム（ID はノード名）と仮想メディア（ID は "<ノード名>/cd"）
//	           を同じノードに並べて宣言できる。
package acceptance

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/internal/redfish/system"
	"github.com/tjst-t/sakutei/pkg/core"
)

func rfMedia(node, image string) string {
	return `{"provider": "` + system.MediaName + `", "id": "` + system.MediaID(node) + `", "fields": {"name": "` + node + `", "image": "` + image + `"}}`
}

func (r *rfRig) media(node string) (bool, string) { return r.bmcs[node].Media() }

func TestRedfishMedia_InsertSwapEject(t *testing.T) {
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{},
		rfMedia("node1", "https://repo.example/a.iso"), reconcile.Config{})

	// [AC-82-1]
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], `"Image":"https://repo.example/a.iso"`) {
		t.Fatalf("[AC-82-1] cycle 1 writes = %v, want one InsertMedia of a.iso", w)
	}
	if in, img := r.media("node1"); !in || img != "https://repo.example/a.iso" {
		t.Fatalf("[AC-82-1] media = %v %q", in, img)
	}
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-82-1] cycle 2 writes = %v, want none (converged)", w)
	}

	// [AC-82-2]
	r.writeIntent(rfMedia("node1", "/isos/b.iso"))
	r.mustCycle()
	w := r.writes("node1")
	if len(w) != 2 || w[0] != "POST {}" || !strings.Contains(w[1], `"Image":"/isos/b.iso"`) {
		t.Fatalf("[AC-82-2] writes = %v, want EjectMedia then InsertMedia of b.iso in the same cycle", w)
	}
	if in, img := r.media("node1"); !in || img != "/isos/b.iso" {
		t.Fatalf("[AC-82-2] media = %v %q", in, img)
	}
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-82-2] writes after the swap = %v, want none", w)
	}

	// [AC-82-3]
	r.writeIntent(rfMedia("node1", ""))
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || w[0] != "POST {}" {
		t.Fatalf("[AC-82-3] writes = %v, want one EjectMedia", w)
	}
	if in, _ := r.media("node1"); in {
		t.Fatalf("[AC-82-3] media is still inserted")
	}
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-82-3] writes after eject = %v, want none", w)
	}
}

func TestRedfishMedia_ReinsertsAfterFetchFailure(t *testing.T) {
	// [AC-82-4]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{MediaFetchDelay: 10 * time.Second},
		rfMedia("node1", "https://repo.example/a.iso"), reconcile.Config{})
	r.bmcs["node1"].SetMediaFetchFails(true) // 取得に失敗する
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], "a.iso") {
		t.Fatalf("[AC-82-4] cycle 1 writes = %v, want InsertMedia", w)
	}
	r.clk.Advance(time.Minute) // BMC が Inserted を false に戻した
	if in, img := r.media("node1"); in || img == "" {
		t.Fatalf("[AC-82-4] fake BMC did not drop the media: %v %q", in, img)
	}
	r.bmcs["node1"].SetMediaFetchFails(false) // 取得元が直った
	r.rec.mu.Lock()
	before := len(r.rec.evs)
	r.rec.mu.Unlock()
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], `"Image":"https://repo.example/a.iso"`) {
		t.Fatalf("[AC-82-4] writes = %v, want InsertMedia again (no Eject: it is not inserted)", w)
	}
	r.rec.mu.Lock()
	evs := append([]core.Event(nil), r.rec.evs[before:]...)
	r.rec.mu.Unlock()
	drift := false
	for _, ev := range evs {
		if ev.Kind == core.EventDrift && ev.Resource.Provider == system.MediaName && ev.Resource.Local == "node1/cd" {
			drift = true
		}
	}
	if !drift {
		t.Fatalf("[AC-82-4] the drop was not reported as a drift; events: %+v", evs)
	}
	r.clk.Advance(time.Minute)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-82-4] writes once the media stays = %v, want none", w)
	}
	if in, _ := r.media("node1"); !in {
		t.Fatalf("[AC-82-4] media not inserted")
	}
}

func TestRedfishMedia_UnreachableIsUnknownAndNoOp(t *testing.T) {
	// [AC-82-5]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{},
		rfMedia("node1", "/isos/a.iso"), reconcile.Config{})
	r.bmcs["node1"].SetUnreachable(true)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-82-5] writes while unreachable = %v, want none", w)
	}
	if !strings.Contains(r.errOut.String(), "observe "+system.MediaName+"//node1/cd") {
		t.Fatalf("[AC-82-5] no unobservable report:\n%s", r.errOut.String())
	}
	r.bmcs["node1"].SetUnreachable(false)
	r.bmcs["node1"].SetFailStatus(http.StatusServiceUnavailable)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-82-5] writes on 5xx = %v, want none", w)
	}
	r.bmcs["node1"].SetFailStatus(0)
	r.mustCycle()
	if in, img := r.media("node1"); !in || img != "/isos/a.iso" {
		t.Fatalf("[AC-82-5] not converged once reachable again: %v %q", in, img)
	}
}

func TestRedfishMedia_NotDestructive(t *testing.T) {
	// [AC-82-6]
	r := newRFRig(t, []string{"node1", "node2"}, redfishtest.Options{},
		rfMedia("node1", "/isos/a.iso")+", "+rfMedia("node2", "/isos/a.iso"),
		reconcile.Config{BudgetConfig: budget.Config{MaxDestructivePerCycle: 1}})
	r.mustCycle()
	r.writeIntent(rfMedia("node1", "/isos/b.iso") + ", " + rfMedia("node2", ""))
	res := r.mustCycle()
	if res.BudgetHalted {
		t.Fatalf("[AC-82-6] media operations halted the destructive budget: %s", res.BudgetReason)
	}
	if in, img := r.media("node1"); !in || img != "/isos/b.iso" {
		t.Fatalf("[AC-82-6] node1 media = %v %q", in, img)
	}
	if in, _ := r.media("node2"); in {
		t.Fatalf("[AC-82-6] node2 media still inserted")
	}
}

func TestRedfishMedia_AlongsideSystem(t *testing.T) {
	// [AC-82-7]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{PowerState: "Off"},
		rfResource("node1", `"power": "On", "boot_enabled": "Continuous", "boot_target": "Cd"`)+", "+rfMedia("node1", "/isos/a.iso"),
		reconcile.Config{})
	r.mustCycle()
	if in, img := r.media("node1"); !in || img != "/isos/a.iso" || r.bmcs["node1"].PowerState() != "On" {
		t.Fatalf("[AC-82-7] media = %v %q, power %s", in, img, r.bmcs["node1"].PowerState())
	}
	r.writes("node1")
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-82-7] cycle 2 writes = %v, want none", w)
	}
}
