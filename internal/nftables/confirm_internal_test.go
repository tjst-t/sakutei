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

package nftables

import (
	"strings"
	"testing"
)

func TestParseConfirm_ObserveCheck(t *testing.T) {
	// Backlog #214: observe: and check: are read as netconf reads them, and
	// are always confirmed after the change.
	ts, err := parseConfirm(`self, observe:probe/site=a,rack=1/p1 expect:{"state":"a, b"},after:tcp://192.0.2.1:443, check:fwd.seen, observe:nftables//fw2/inet/filter/web,ssh://198.51.100.20`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 6 {
		t.Fatalf("targets %v", ts)
	}
	o := ts[1]
	if o.Kind != ConfirmObserve || o.ID.Scope.Labels["rack"] != "1" || o.ID.Local != "p1" || o.Expect["state"] != "a, b" || !o.After {
		t.Fatalf("observe = %+v", o)
	}
	if ts[2].Kind != ConfirmTCP || !ts[2].After || ts[3].Kind != ConfirmCheck || ts[3].Check != "fwd.seen" || !ts[3].After {
		t.Fatalf("targets %+v", ts)
	}
	if ts[4].ID.Local != "fw2/inet/filter/web" || ts[5].Kind != ConfirmSSH {
		t.Fatalf("targets %+v", ts)
	}
	if got := len(reachTargets(ts)); got != 3 {
		t.Fatalf("reach targets %d, want 3", got)
	}
	if got := beforeTargets(reachTargets(ts)); len(got) != 2 {
		t.Fatalf("before targets %v", got)
	}
	// The written form reads back the same.
	var parts []string
	for _, x := range ts {
		parts = append(parts, x.String())
	}
	again, err := parseConfirm(strings.Join(parts, ", "))
	if err != nil || len(again) != 6 || again[1].Expect["state"] != "a, b" {
		t.Fatalf("round trip: %v %v", again, err)
	}
	if strings.Contains(again[3].String(), "after:") {
		t.Fatalf("check: is written without after: %q", again[3])
	}
	for in, want := range map[string]string{
		"check:x":                          "no target is reached",
		"observe:p//x":                     "no target is reached",
		"after:tcp://192.0.2.1:1, check:x": "every target is marked after:",
		"self check:x":                     "want self",
		"self, check:x, check:x":           "listed twice",
		"self, observe:p//x expect:{}":     "expects nothing",
		"self, observe:p/x":                "want observe:<provider>/<scope>/<id>",
		"self, check:-x":                   "check name",
	} {
		if _, err := parseConfirm(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseConfirm(%q) = %v, want %q", in, err, want)
		}
	}
	// Without a confirmer, observe: and check: cannot be checked at load.
	f := map[string]string{"type": "filter", "hook": "input", "priority": "0", "policy": "drop", FieldConfirm: "self, check:x"}
	if err := validateConfirm(f, "inet/filter/input", false, nil); err == nil || !strings.Contains(err.Error(), "no confirmer") {
		t.Fatalf("validateConfirm = %v", err)
	}
}
