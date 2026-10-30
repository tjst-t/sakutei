//go:build integration_nftables

package nftables

// confirm_check_integration_test.go — confirm の先の observe:（別のリソースの
// 観測、Backlog #214）を、本物の nft と決まった道具 sakutei-nft-guard
// （Backlog #191）で確かめる。
//
// ホストの資格は書き込み用（sakutei）と観測用（sakutei-ro。sudo は道具の list
// だけ）に分け、sudo の記録をファイルに残す。base chain（input）を変える宣言の
// confirm に、同じホストの別の chain（web）の観測を書く:
//
//   - web が expect どおり: 変えて、巻き戻しを取り消す（確定）。apply から
//     disarm までの間の読みは観測用の利用者の list だけ（書き込み用の資格を
//     使わない）。
//   - web が expect と違う: 確定せず、道具の巻き戻しが元の table に戻す
//     （reconcile は 4、知らせに observe: の先）。

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const confirmCheckContainer = "sakutei-nftit-confirmcheck"

func TestNFTablesConfirmObserve(t *testing.T) {
	l := setupSSHLabIn(t, confirmCheckContainer)
	l.installGuardTool(t)
	sudoLog := "/var/log/sakutei-sudo.log"
	l.gsh(t, "printf 'Defaults logfile="+sudoLog+"\\nDefaults loglinelen=0\\n' > /etc/sudoers.d/zz-log && chmod 0440 /etc/sudoers.d/zz-log && visudo -c >/dev/null")
	// The minimal core kept outside Sakutei.
	l.gsh(t, "nft add table inet sk && nft add chain inet sk mgmt && nft add rule inet sk mgmt tcp dport 22 accept")
	skTable := func() string { return l.gsh(t, "nft -s list table inet sk") }

	web := map[string]any{"provider": "nftables", "id": "fw/inet/sk/web", "scope": labScope, "fields": map[string]string{
		"family": "inet", "table": "sk", "chain": "web", "rules": "tcp dport 80 accept",
	}}
	declare := func(confirm string, rules ...string) {
		t.Helper()
		res := []map[string]any{web}
		if confirm != "" {
			res = append(res, map[string]any{"provider": "nftables", "id": "fw/inet/sk/input", "scope": labScope, "fields": map[string]string{
				"family": "inet", "table": "sk", "chain": "input", "rules": strings.Join(rules, " ; "),
				"type": "filter", "hook": "input", "priority": "filter", "policy": "drop", "confirm": confirm,
			}})
		}
		writeJSONFile(t, filepath.Join(l.intent, "fw.json"), map[string]any{
			"external_owners": []map[string]any{{"owner": "stage0", "provider": "nftables", "id": "fw/inet/sk/mgmt", "scope": labScope}},
			"planned_change":  approvedLanes(),
			"resources":       res,
		})
	}
	observeWeb := func(rules string) string {
		return `self, observe:nftables/site=lab/fw/inet/sk/web expect:{"rules":"` + rules + `"}`
	}
	reconcile := func(t *testing.T) (string, int) {
		t.Helper()
		return l.run(t, "reconcile", "--apply-check-state", l.state)
	}

	t.Run("the observed chain is there", func(t *testing.T) {
		declare("")
		if out, code := reconcile(t); code != 0 || !strings.Contains(skTable(), "chain web") {
			t.Fatalf("reconcile exited %d:\n%s\n%s", code, out, skTable())
		}
	})

	t.Run("observed as expected: changed and confirmed", func(t *testing.T) {
		declare(observeWeb("tcp dport 80 accept"), "ct state established,related accept", "jump mgmt", "jump web")
		l.gsh(t, ": > "+sudoLog)
		start := time.Now()
		out, code := reconcile(t)
		if code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if took := time.Since(start); took >= rollbackAfter {
			t.Errorf("the confirmed Apply took %s (it waited for the rollback)", took)
		}
		if p := policyIn(t, l.container); p != "drop" {
			t.Fatalf("policy = %q, want drop\n%s", p, out)
		}
		// Between the apply and the disarm, the tool's list ran as the
		// observe user only: the observe: read with the observe credential.
		lines := strings.Split(l.gsh(t, "cat "+sudoLog), "\n")
		applyAt, disarmAt := -1, -1
		for i, ln := range lines {
			switch {
			case strings.Contains(ln, " : sakutei : ") && strings.Contains(ln, "COMMAND="+guardTool+" apply"):
				applyAt = i
			case applyAt >= 0 && disarmAt < 0 && strings.Contains(ln, "COMMAND="+guardTool+" disarm"):
				disarmAt = i
			}
		}
		if applyAt < 0 || disarmAt < 0 {
			t.Fatalf("no apply and disarm in the sudo log:\n%s", strings.Join(lines, "\n"))
		}
		ro := 0
		for _, ln := range lines[applyAt+1 : disarmAt] {
			if !strings.Contains(ln, " : sakutei-ro : ") || !strings.Contains(ln, "COMMAND="+guardTool+" list inet sk") {
				t.Fatalf("between the apply and the disarm: %q (want only the observe user's list)", ln)
			}
			ro++
		}
		if ro == 0 {
			t.Fatalf("the observe: did not read web between the apply and the disarm:\n%s", strings.Join(lines, "\n"))
		}
		time.Sleep(rollbackAfter + 3*time.Second)
		if p := policyIn(t, l.container); p != "drop" {
			t.Fatalf("policy went back to %q after the rollback time (the rollback was not cancelled)", p)
		}
	})
	confirmed := skTable()

	t.Run("observed not as expected: rolled back", func(t *testing.T) {
		declare(observeWeb("tcp dport 8080 accept"), "ct state established,related accept", "jump mgmt", "jump web", "tcp dport 443 accept")
		out, code := reconcile(t)
		if code != 4 || !strings.Contains(out, "rolled back") || !strings.Contains(out, "observe:nftables/site=lab/fw/inet/sk/web: rules not as expected") {
			t.Fatalf("reconcile exited %d, want 4 rolled back naming the observe: target:\n%s", code, out)
		}
		if got := skTable(); got != confirmed {
			t.Fatalf("not rolled back:\n%s\nwant:\n%s", got, confirmed)
		}
		if !l.freshSSH() {
			t.Fatal("the host is not reachable after the rollback")
		}
	})

	t.Run("no reached target: refused on loading", func(t *testing.T) {
		declare(`observe:nftables/site=lab/fw/inet/sk/web`, "jump mgmt")
		out, code := l.run(t, "plan")
		if code == 0 || !strings.Contains(out, "no target is reached") {
			t.Fatalf("plan exited %d, want the declaration refused:\n%s", code, out)
		}
		if got := skTable(); got != confirmed {
			t.Fatalf("the table changed:\n%s", got)
		}
	})
}
