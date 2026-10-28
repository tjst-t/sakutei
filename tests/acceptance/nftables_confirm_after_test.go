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

package acceptance

// nftables_confirm_after_test.go — #192: confirm の先ごとの「変えたあとだけ
// 確かめる」印（after:）。変える chain を通る経路の先（forward を通る別の経路の
// ssh）は、空からの初回は変える前に届かない。偽の nft と偽の守りで。
//
//	[AC-192-1]  印の付いた先は変える前に確かめず、空から 1 回で変わり、変えた
//	            あとに確かめる（前は self だけ、後は両方）。
//	[AC-192-2]  同じ先に印が無ければ、いまどおり変える前に止まる。
//	[AC-192-3]  変えたあとに印の付いた先に届かなければ巻き戻す（知らせに印つき
//	            の先を名指し）。
//	[AC-192-4]  読み込み: 全部に印の付いた宣言・after:self・印の有無だけ違う
//	            重複は断る。
//	[AC-192-5]  plan の op の confirm と、前の確かめの知らせに印が出る。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const forwardSSH = "ssh://admin@198.51.100.20:22"

// forwardLab は空の table と、forward の base chain が 22 を通すときだけ届く
// 別の経路の ssh（forwardSSH）の守り。
func forwardLab(t *testing.T) (*nftfake.NFT, *nftfake.Guard, *nftables.LiveProvider) {
	t.Helper()
	fake := &nftfake.NFT{}
	guard := &nftfake.Guard{NFT: fake, UnreachableTo: func(n *nftfake.NFT, to nftables.ConfirmTarget) bool {
		return to.Kind == nftables.ConfirmSSH && !n.HasRule("inet", "filter", "forward", "tcp dport 22 accept")
	}}
	return fake, guard, labLive(t, fake, guard)
}

func forwardFields(confirm, rules string) map[string]string {
	f := baseFields("forward", "forward", "drop", rules)
	f[nftables.FieldConfirm] = confirm
	return f
}

func TestNFTablesConfirmAfter_FromEmpty(t *testing.T) {
	// [AC-192-1]
	fake, guard, p := forwardLab(t)
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{createOp("fw1/inet/filter/forward", forwardFields("self, after:"+forwardSSH, "ct state established,related accept ; tcp dport 22 accept"))}})
	if err != nil {
		t.Fatalf("[AC-192-1] Apply: %v", err)
	}
	if fake.Policy("inet", "filter", "forward") != "drop" {
		t.Fatalf("[AC-192-1] not changed:\n%s", fake.Dump())
	}
	want := []string{"self", "self", "after:" + forwardSSH}
	if got := guard.Confirms(); !reflect.DeepEqual(got, want) {
		t.Fatalf("[AC-192-1] confirms = %v, want %v (self before, both after)", got, want)
	}
	if arms, disarms, fires := guard.Counts(); arms != 1 || disarms != 1 || fires != 0 {
		t.Fatalf("[AC-192-1] arms/disarms/fires = %d/%d/%d, want 1/1/0", arms, disarms, fires)
	}
}

func TestNFTablesConfirmAfter_UnmarkedStops(t *testing.T) {
	// [AC-192-2] [AC-192-5]
	fake, guard, p := forwardLab(t)
	before := fake.Dump()
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{createOp("fw1/inet/filter/forward", forwardFields("self, "+forwardSSH, "tcp dport 22 accept"))}})
	var te *provider.TransientError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "before the change") || !strings.Contains(err.Error(), forwardSSH) {
		t.Fatalf("[AC-192-2] Apply = %v, want stopped before the change naming %s", err, forwardSSH)
	}
	if arms, _, _ := guard.Counts(); arms != 0 || fake.Dump() != before {
		t.Fatalf("[AC-192-2] changed: arms %d\n%s", arms, fake.Dump())
	}

	// A marked target with self not reached: stopped, and the notice tells
	// the after: target apart.
	fake2 := &nftfake.NFT{}
	guard2 := &nftfake.Guard{NFT: fake2, UnreachableTo: func(_ *nftfake.NFT, to nftables.ConfirmTarget) bool { return to.Kind == nftables.ConfirmSelf }}
	err = labLive(t, fake2, guard2).Apply(context.Background(), core.Plan{Ops: []core.Op{createOp("fw1/inet/filter/forward", forwardFields("self, after:"+forwardSSH, "tcp dport 22 accept"))}})
	if err == nil || !strings.Contains(err.Error(), "after:"+forwardSSH) || !strings.Contains(err.Error(), "confirmed only after the change") {
		t.Fatalf("[AC-192-5] Apply = %v, want the notice to show the after: mark", err)
	}
	if got := guard2.Confirms(); !reflect.DeepEqual(got, []string{"self"}) {
		t.Fatalf("[AC-192-5] confirms before the change = %v, want only self", got)
	}
}

func TestNFTablesConfirmAfter_RolledBack(t *testing.T) {
	// [AC-192-3]
	fake, guard, p := forwardLab(t)
	before := fake.Dump()
	// The declaration does not let the forwarded ssh through.
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{createOp("fw1/inet/filter/forward", forwardFields("self, after:"+forwardSSH, "tcp dport 80 accept"))}})
	var rb *provider.RolledBackError
	if !errors.As(err, &rb) || !strings.Contains(err.Error(), "after:"+forwardSSH) {
		t.Fatalf("[AC-192-3] Apply = %v, want rolled back naming after:%s", err, forwardSSH)
	}
	if fake.Dump() != before {
		t.Fatalf("[AC-192-3] the ruleset is not back:\n%s", fake.Dump())
	}
	if arms, disarms, fires := guard.Counts(); arms != 1 || disarms != 0 || fires != 1 {
		t.Fatalf("[AC-192-3] arms/disarms/fires = %d/%d/%d, want 1/0/1", arms, disarms, fires)
	}
}

func TestNFTablesConfirmAfter_Validate(t *testing.T) {
	// [AC-192-4]
	p := labLive(t, &nftfake.NFT{}, nil)
	for confirm, wantErr := range map[string]string{
		"self, after:" + forwardSSH:                     "",
		"tcp://192.0.2.10:443, after:" + forwardSSH:     "",
		"after:" + forwardSSH:                           "every target is marked after:",
		"after:tcp://192.0.2.1:1, after:" + forwardSSH:  "every target is marked after:",
		"after:self, " + forwardSSH:                     "self is Sakutei's own management connection",
		"self, " + forwardSSH + ", after:" + forwardSSH: "listed twice",
		"self, after:":                                  "want self",
	} {
		err := p.ValidateIntent("fw1/inet/filter/forward", forwardFields(confirm, "accept"))
		switch {
		case wantErr == "" && err != nil:
			t.Errorf("[AC-192-4] %q: %v", confirm, err)
		case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
			t.Errorf("[AC-192-4] %q: %v, want %q", confirm, err, wantErr)
		}
	}
}

func TestNFTablesConfirmAfter_Plan(t *testing.T) {
	// [AC-192-5]
	_, _, p := forwardLab(t)
	obs, err := p.Read(context.Background(), "fw1/inet/filter/forward")
	if err != nil {
		t.Fatal(err)
	}
	fields := forwardFields("self, after:"+forwardSSH, "tcp dport 22 accept")
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: core.NewID("nftables", core.NewScope(nftLabScope), "fw1/inet/filter/forward"), Fields: fields}, cause.CauseContext{})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range pl.Ops {
		for _, f := range op.Fields {
			if f.Field == nftables.FieldConfirm && strings.Contains(f.To, "after:"+forwardSSH) {
				return
			}
		}
	}
	t.Fatalf("[AC-192-5] the plan does not show the after: mark: %+v", pl.Ops)
}
