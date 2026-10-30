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

// commit_group（#199）を netopeer2 の 3 台（コンテナ sakutei-ncit-g1〜g3、
// TestMain が立てる）で確かめる。3 台の部分木 acl-rules を commit_group
// "leafs" にまとめ、
//
//   - 全部の台で確かめが通れば、3 台とも確定する（confirm-timeout の後も
//     宣言どおり。台ごとに CommitGroupApplied の committed）。
//   - g3 だけを、Sakutei 自身が NACM を読めなくなる宣言（admin-rules に
//     deny-read-nacm）にすると、g3 の確かめが落ち、どの台も確定しない:
//     g1・g2 も手で変えたままに戻る。netopeer2 は :confirmed-commit:1.1 を
//     持つので、cancel-commit ですぐ戻す。
//
// 時間の上限は一番短い confirm-timeout（g1・g3 の 10 秒。g2 は 20 秒）の半分。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func groupRunning(t *testing.T, g string) string {
	t.Helper()
	return netconfByHandOn(t, lab.group[g], `<get-config><source><running/></source><filter type="subtree"><nacm xmlns="`+nacmNS+`"/></filter></get-config>`)[0]
}

// declaredOrder reports whether the device's rule-list sakutei-it has r1
// before r2 (as declared).
func declaredOrder(r string) bool {
	i, j := strings.Index(r, "<name>r1</name>"), strings.Index(r, "<name>r2</name>")
	return i >= 0 && j >= 0 && i < j
}

func TestNetconfCommitGroupIntegration(t *testing.T) {
	groupConfig := filepath.Join(lab.dir, "providers-group.json")
	changeAll := func(t *testing.T) {
		t.Helper()
		for _, g := range groupDevices {
			r := netconfByHandOn(t, lab.group[g], `<edit-config><target><running/></target><config><nacm xmlns="`+nacmNS+`" xmlns:nc="`+baseNS+`">`+
				group("alice", "bob")+strings.Replace(ruleList(ruleR2, ruleR1), "<rule-list>", `<rule-list nc:operation="replace">`, 1)+`</nacm></config></edit-config>`)
			if !strings.Contains(r[0], "<ok/>") {
				t.Fatalf("edit-config by hand on %s: %s", g, r[0])
			}
			if declaredOrder(groupRunning(t, g)) {
				t.Fatalf("%s: the hand change did not take", g)
			}
		}
	}
	writeGroupIntent := func(t *testing.T, dir string, lockoutG3 bool) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		var res []map[string]any
		for _, g := range groupDevices {
			if g == "g3" && lockoutG3 {
				res = append(res, map[string]any{"provider": "netconf", "id": "g3/admin-rules", "fields": map[string]string{
					"config": `<nacm xmlns="` + nacmNS + `"><rule-list><name>sakutei-it-admin</name><group>sakutei-it-admin</group>` +
						`<rule><name>deny-read-nacm</name><module-name>ietf-netconf-acm</module-name><access-operations>read</access-operations><action>deny</action></rule>` +
						`<rule><name>permit-all</name><module-name>*</module-name><access-operations>*</access-operations><action>permit</action></rule></rule-list></nacm>`,
					"ordered":      "nacm/rule-list/rule",
					"commit_group": "leafs"}})
				continue
			}
			res = append(res, map[string]any{"provider": "netconf", "id": g + "/acl-rules", "fields": map[string]string{
				"config":       `<nacm xmlns="` + nacmNS + `">` + ruleList(ruleR1, ruleR2) + `</nacm>`,
				"ordered":      "nacm/rule-list/rule",
				"commit_group": "leafs"}})
		}
		if err := writeJSON(filepath.Join(dir, "netconf.json"), map[string]any{"resources": res}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("every check passes: all three confirmed", func(t *testing.T) {
		changeAll(t)
		dir := filepath.Join(lab.dir, "intent-group")
		writeGroupIntent(t, dir, false)
		out, errOut, _ := sakuteiWith(t, dir, groupConfig, "plan", "--json")
		if strings.Count(out, `"commit_group": "leafs"`) != 3 {
			t.Fatalf("plan --json does not name the group of the 3 operations:\n%s%s", out, errOut)
		}
		out, errOut, code := sakuteiWith(t, dir, groupConfig, "reconcile")
		t.Logf("sakutei reconcile (commit group, all pass):\n%s%s", out, errOut)
		if code != 0 || !strings.Contains(out, "3 change(s) applied") {
			t.Fatalf("reconcile: exit %d\n%s%s", code, out, errOut)
		}
		for _, g := range groupDevices {
			if !strings.Contains(out+errOut, `commit_group "leafs": netconf//`+g+`/acl-rules: committed`) {
				t.Fatalf("no committed event for %s:\n%s%s", g, out, errOut)
			}
			if !declaredOrder(groupRunning(t, g)) {
				t.Fatalf("%s not as declared", g)
			}
		}
		// Confirmed: still as declared after the shortest confirm-timeout.
		time.Sleep(confirmTimeout + 2*time.Second)
		for _, g := range groupDevices {
			if !declaredOrder(groupRunning(t, g)) {
				t.Fatalf("%s rolled back after the confirm-timeout", g)
			}
		}
		out, errOut, code = sakuteiWith(t, dir, groupConfig, "reconcile")
		if code != 0 || !strings.Contains(out, "Converged") {
			t.Fatalf("reconcile after: exit %d\n%s%s", code, out, errOut)
		}
	})

	t.Run("g3's check fails: no device confirmed", func(t *testing.T) {
		changeAll(t)
		before := groupRunning(t, "g3")
		dir := filepath.Join(lab.dir, "intent-group-lockout")
		writeGroupIntent(t, dir, true)
		out, errOut, _ := sakuteiWith(t, dir, groupConfig, "reconcile")
		t.Logf("sakutei reconcile (commit group, g3 locked out):\n%s%s", out, errOut)
		for _, id := range []string{"g1/acl-rules", "g2/acl-rules", "g3/admin-rules"} {
			if !strings.Contains(errOut, `commit_group "leafs": netconf//`+id+`: not committed`) {
				t.Fatalf("no not-committed event for %s:\n%s%s", id, out, errOut)
			}
		}
		for _, want := range []string{"g3/admin-rules was not confirmed", "cancel-commit was sent"} {
			if !strings.Contains(errOut, want) {
				t.Fatalf("reconcile does not say %q:\n%s%s", want, out, errOut)
			}
		}
		for _, g := range []string{"g1", "g2"} {
			if declaredOrder(groupRunning(t, g)) {
				t.Fatalf("%s was committed although g3's check failed", g)
			}
		}
		r := groupRunning(t, "g3")
		if strings.Contains(r, "deny-read-nacm") || r != before {
			t.Fatalf("g3 not rolled back:\n%s", r)
		}
	})
}
