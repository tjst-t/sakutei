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

//go:build integration_netconf

package netconf

// confirm の先の observe:（#200）を netopeer2 の 2 台（commit_group の試験の
// g1・g2 のコンテナ）で確かめる。g1 の部分木 acl-rules を変え、その確定の
// 条件を「g2 の acl-rules が r1, r2 の順であること」（observe: と expect:）に
// する:
//
//   - g2 がその順なら、g1 は確定する（confirm-timeout の後も宣言どおり）。
//   - g2 が逆の順なら、g1 は確定しない: 確定の commit を送らず、機器の
//     confirm-timeout で g1 は手で変えたままに戻る。g2 は読むだけ（g2 の宣言は
//     今の running どおりなので変えない）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setOrder は g の rule-list sakutei-it を手で rules の順にする。
func setOrder(t *testing.T, g string, rules ...string) {
	t.Helper()
	r := netconfByHandOn(t, lab.group[g], `<edit-config><target><running/></target><config><nacm xmlns="`+nacmNS+`" xmlns:nc="`+baseNS+`">`+
		strings.Replace(ruleList(rules...), "<rule-list>", `<rule-list nc:operation="replace">`, 1)+`</nacm></config></edit-config>`)
	if !strings.Contains(r[0], "<ok/>") {
		t.Fatalf("edit-config by hand on %s: %s", g, r[0])
	}
}

func TestNetconfConfirmObserveIntegration(t *testing.T) {
	config := filepath.Join(lab.dir, "providers-group.json")
	declared := `<nacm xmlns="` + nacmNS + `">` + ruleList(ruleR1, ruleR2) + `</nacm>`
	reversed := `<nacm xmlns="` + nacmNS + `">` + ruleList(ruleR2, ruleR1) + `</nacm>`
	writeConfirmIntent := func(t *testing.T, dir, g2Running string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		var expect strings.Builder
		enc := json.NewEncoder(&expect)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]string{"config": declared}); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "netconf.json"), map[string]any{"resources": []map[string]any{
			{"provider": "netconf", "id": "g1/acl-rules", "fields": map[string]string{
				"config":  declared,
				"ordered": "nacm/rule-list/rule",
				"confirm": "self, observe:netconf//g2/acl-rules expect:" + strings.TrimSpace(expect.String())}},
			// g2 is declared as it is: it is only read.
			{"provider": "netconf", "id": "g2/acl-rules", "fields": map[string]string{
				"config":  g2Running,
				"ordered": "nacm/rule-list/rule"}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the peer is as expected: g1 confirmed", func(t *testing.T) {
		setOrder(t, "g1", ruleR2, ruleR1)
		setOrder(t, "g2", ruleR1, ruleR2)
		dir := filepath.Join(lab.dir, "intent-confirm-ok")
		writeConfirmIntent(t, dir, declared)
		out, errOut, code := sakuteiWith(t, dir, config, "reconcile")
		t.Logf("sakutei reconcile (observe: as expected):\n%s%s", out, errOut)
		if code != 0 || !strings.Contains(out, "1 change(s) applied") {
			t.Fatalf("reconcile: exit %d\n%s%s", code, out, errOut)
		}
		if !declaredOrder(groupRunning(t, "g1")) {
			t.Fatal("g1 not as declared")
		}
		time.Sleep(confirmTimeout + 2*time.Second)
		if !declaredOrder(groupRunning(t, "g1")) {
			t.Fatal("g1 rolled back after the confirm-timeout")
		}
	})

	t.Run("the peer is not as expected: g1 not confirmed", func(t *testing.T) {
		setOrder(t, "g1", ruleR2, ruleR1)
		setOrder(t, "g2", ruleR2, ruleR1)
		g2Before := groupRunning(t, "g2")
		dir := filepath.Join(lab.dir, "intent-confirm-ng")
		writeConfirmIntent(t, dir, reversed)
		out, errOut, _ := sakuteiWith(t, dir, config, "reconcile")
		t.Logf("sakutei reconcile (observe: not as expected):\n%s%s", out, errOut)
		for _, want := range []string{"not confirmed: observe:netconf//g2/acl-rules: config not as expected", "the confirming commit was not sent"} {
			if !strings.Contains(errOut, want) {
				t.Fatalf("reconcile does not say %q:\n%s%s", want, out, errOut)
			}
		}
		if declaredOrder(groupRunning(t, "g1")) {
			t.Fatal("g1 was committed although g2 was not as expected")
		}
		if got := groupRunning(t, "g2"); got != g2Before {
			t.Fatalf("g2 was changed:\n%s", got)
		}
	})
}
