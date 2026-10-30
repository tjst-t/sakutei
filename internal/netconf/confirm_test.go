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

package netconf

import (
	"strings"
	"testing"
)

func TestParseConfirm(t *testing.T) {
	ts, err := ParseConfirm(`self, observe:netconf/site=a,rack=1/sw2/if-uplink expect:{"config":"<a xmlns=\"urn:x\">1, 2</a>"}, after:check:route.seen, observe:probe//p1,check:c2`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 5 {
		t.Fatalf("got %d targets: %v", len(ts), ts)
	}
	o := ts[1]
	if o.Kind != ConfirmObserve || o.ID.Provider != "netconf" || o.ID.Scope.Labels["rack"] != "1" || o.ID.Local != "sw2/if-uplink" ||
		o.Expect["config"] != `<a xmlns="urn:x">1, 2</a>` {
		t.Fatalf("observe: %+v", o)
	}
	if ts[2].Kind != ConfirmCheck || ts[2].Check != "route.seen" || ts[3].ID.Local != "p1" || ts[3].Expect != nil || ts[4].Check != "c2" {
		t.Fatalf("targets: %+v", ts)
	}
	if ts, err := ParseConfirm(" "); err != nil || ts != nil {
		t.Fatalf("empty: %v %v", ts, err)
	}
	for in, want := range map[string]string{
		"check:x":                           "no target is reached",
		"after:observe:p//x":                "no target is reached",
		"self, after:self":                  "after:self",
		"self, self":                        "listed twice",
		"self, ssh://h":                     "want self, observe:",
		"self check:x":                      "want a comma",
		"self, observe:netconf/sw2":         "want observe:<provider>/<scope>/<id>",
		"self, observe:p//x expect:[1]":     "want a JSON object",
		"self, observe:p//x expect:{}":      "expects nothing",
		`self, observe:p//x expect:{"a":1}`: "want a string value",
		"self, check:-x":                    "check name",
	} {
		if _, err := ParseConfirm(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseConfirm(%q) = %v, want %q", in, err, want)
		}
	}
}

func TestConfirmTargetString_RoundTrip(t *testing.T) {
	in := `self, observe:netconf//sw2/x expect:{"b":"2","a":"1, 3"}, check:c`
	ts, err := ParseConfirm(in)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, x := range ts {
		parts = append(parts, x.String())
	}
	again, err := ParseConfirm(strings.Join(parts, ", "))
	if err != nil || len(again) != 3 || again[1].Expect["a"] != "1, 3" {
		t.Fatalf("round trip: %v %v", again, err)
	}
}
