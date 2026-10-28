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

//go:build integration_nftables

package nftables

// 使い捨ての network namespace は `sudo -n unshare -n sh -c 'echo $$; exec cat'`
// で作る。cat は標準入力が閉じるまで生き、テストのプロセスが（落ちても）終われば
// パイプが閉じて namespace ごと消える。nft はその namespace の中で
// `sudo -n nsenter -t <pid> -n nft ...` として走らせる（ラッパーのスクリプトを
// --ruleset-nft に渡す）ので、sakutei は本物の ExecRunner で本物の nft を呼ぶ。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var lab struct {
	dir     string // 作業ディレクトリ
	bin     string // sakutei のバイナリ
	wrapper string // namespace の中の nft を呼ぶラッパー
	config  string // --provider-config
	holder  *exec.Cmd
	stdin   io.WriteCloser
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-nftables: %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	return m.Run()
}

func setup() error {
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		return fmt.Errorf("sudo without a password is required: %w", err)
	}
	out, err := exec.Command("sudo", "-n", "sh", "-c", "command -v nft").Output()
	if err != nil {
		return fmt.Errorf("nft is not installed: %w", err)
	}
	nft := strings.TrimSpace(string(out))
	for _, tool := range []string{"unshare", "nsenter"} {
		if err := exec.Command("sudo", "-n", "sh", "-c", "command -v "+tool).Run(); err != nil {
			return fmt.Errorf("%s is not installed", tool)
		}
	}

	dir, err := os.MkdirTemp("", "sakutei-nft-it-")
	if err != nil {
		return err
	}
	lab.dir = dir

	// 使い捨ての network namespace。
	lab.holder = exec.Command("sudo", "-n", "unshare", "-n", "sh", "-c", "echo $$; exec cat >/dev/null")
	lab.stdin, err = lab.holder.StdinPipe()
	if err != nil {
		return err
	}
	pidOut, err := lab.holder.StdoutPipe()
	if err != nil {
		return err
	}
	if err := lab.holder.Start(); err != nil {
		return fmt.Errorf("unshare: %w", err)
	}
	line, err := bufio.NewReader(pidOut).ReadString('\n')
	if err != nil {
		return fmt.Errorf("unshare: read pid: %w", err)
	}
	pid := strings.TrimSpace(line)

	lab.wrapper = filepath.Join(dir, "nft-in-netns")
	script := fmt.Sprintf("#!/bin/sh\nexec sudo -n nsenter -t %s -n %s \"$@\"\n", pid, nft)
	if err := os.WriteFile(lab.wrapper, []byte(script), 0o755); err != nil {
		return err
	}
	// ホストの namespace と違うことを確かめる（取り違えてホストの FW に触れない）。
	self, err := exec.Command("sudo", "-n", "readlink", "/proc/1/ns/net").Output()
	if err != nil {
		return err
	}
	ns, err := exec.Command("sudo", "-n", "readlink", "/proc/"+pid+"/ns/net").Output()
	if err != nil {
		return err
	}
	if string(self) == string(ns) {
		return fmt.Errorf("the namespace is the host's (%s)", strings.TrimSpace(string(ns)))
	}

	lab.bin = filepath.Join(dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = "../../.."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}

	lab.config = filepath.Join(dir, "providers.json")
	cfg := map[string]any{"providers": map[string]any{"nftables": map[string]any{
		"ruleset_target": "live",
		"ruleset_tables": "inet/sk",
		"ruleset_nft":    lab.wrapper,
	}}}
	b, _ := json.Marshal(cfg)
	return os.WriteFile(lab.config, b, 0o644)
}

func teardown() {
	if lab.stdin != nil {
		lab.stdin.Close()
	}
	if lab.holder != nil && lab.holder.Process != nil {
		_ = lab.holder.Wait()
	}
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
}

// nft は namespace の中で nft を走らせる。
func nft(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(lab.wrapper, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("nft %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// nftScript は namespace の中で nft -f - にスクリプトを流す。
func nftScript(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command(lab.wrapper, "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nft -f: %v\n%s\n%s", err, script, out)
	}
}

type resource struct {
	Provider string            `json:"provider"`
	ID       string            `json:"id"`
	Fields   map[string]string `json:"fields"`
}

func chainRes(chain string, rules ...string) resource {
	return resource{Provider: "nftables", ID: "inet/sk/" + chain, Fields: map[string]string{
		"family": "inet", "table": "sk", "chain": chain, "rules": strings.Join(rules, " ; "),
	}}
}

func writeIntent(t *testing.T, rs ...resource) {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fw.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// sakutei は CLI を 1 回動かして、出力（stdout + stderr）と終了コードを返す。
func sakutei(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{verb,
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--provider-config", lab.config,
	}, extra...)
	out, err := exec.Command(lab.bin, args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %v: %v", args, err)
	}
	return string(out), 0
}

type statusDoc struct {
	Complete bool `json:"complete"`
	Summary  struct {
		Conformant   int `json:"conformant"`
		Drift        int `json:"drift"`
		Unobservable int `json:"unobservable"`
	} `json:"summary"`
	Resources []struct {
		LocalID string `json:"local_id"`
		State   string `json:"state"`
	} `json:"resources"`
}

func status(t *testing.T) statusDoc {
	t.Helper()
	args := []string{"status", "--json",
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--provider-config", lab.config,
	}
	cmd := exec.Command(lab.bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sakutei status: %v\n%s%s", err, out, stderr.String())
	}
	var doc statusDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	return doc
}

func mustReconcile(t *testing.T) string {
	t.Helper()
	out, code := sakutei(t, "reconcile")
	if code != 0 {
		t.Fatalf("reconcile exited %d:\n%s", code, out)
	}
	return out
}

// listChain は chain のルール文（handle 付き）を返す。
func listChain(t *testing.T, chain string) []string {
	t.Helper()
	var rules []string
	for _, l := range strings.Split(nft(t, "-a", "-s", "list", "chain", "inet", "sk", chain), "\n") {
		if l = strings.TrimSpace(l); strings.Contains(l, "# handle") && !strings.HasSuffix(strings.TrimSpace(l[:strings.LastIndex(l, "#")]), "{") {
			rules = append(rules, l)
		}
	}
	return rules
}

var handleRe = regexp.MustCompile(` # handle \d+$`)

func withoutHandles(rules []string) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = handleRe.ReplaceAllString(r, "")
	}
	return out
}

var (
	webRules = []string{
		"tcp dport 22 accept",
		"tcp dport { 80, 443 } ct state established,new counter accept comment \"web\"",
		"ip saddr 10.0.0.0/8 drop",
	}
	sshRules = []string{"ip saddr 192.0.2.0/24 tcp dport 22 accept", "drop"}
)

func TestNFTablesLiveIntegration(t *testing.T) {
	// 管理しない table（どの段でも変わらない）。
	nftScript(t, "add table ip other\nadd chain ip other x\nadd rule ip other x tcp dport 9 accept\n")
	other := nft(t, "list", "table", "ip", "other")

	writeIntent(t, chainRes("web", webRules...), chainRes("ssh", sshRules...))

	t.Run("create from empty", func(t *testing.T) {
		if s := status(t); s.Summary.Drift != 2 || !s.Complete {
			t.Fatalf("before: %+v, want 2 drifted (the table does not exist yet)", s)
		}
		mustReconcile(t)
		if s := status(t); s.Summary.Conformant != 2 || s.Summary.Drift != 0 {
			t.Fatalf("after: %+v, want 2 conformant", s)
		}
		if got := withoutHandles(listChain(t, "web")); strings.Join(got, "|") != strings.Join(webRules, "|") {
			t.Fatalf("web = %q", got)
		}
	})

	t.Run("base chain and set are not touched", func(t *testing.T) {
		nftScript(t, "add set inet sk allowed { type ipv4_addr; elements = { 10.1.1.1 }; }\n"+
			"add chain inet sk input { type filter hook input priority 0; policy accept; }\n"+
			"add rule inet sk input jump web\n")
		input := nft(t, "list", "chain", "inet", "sk", "input")
		set := nft(t, "list", "set", "inet", "sk", "allowed")
		if out, code := sakutei(t, "plan"); code != 0 {
			t.Fatalf("plan exited %d:\n%s", code, out)
		}
		if s := status(t); s.Summary.Drift != 0 || s.Summary.Conformant != 2 {
			t.Fatalf("status: %+v", s)
		}
		mustReconcile(t)
		if nft(t, "list", "chain", "inet", "sk", "input") != input || nft(t, "list", "set", "inet", "sk", "allowed") != set {
			t.Fatal("the base chain or the set changed")
		}
	})

	t.Run("a rule deleted by hand is drift and comes back", func(t *testing.T) {
		rules := listChain(t, "web")
		h := handleRe.FindString(rules[1])
		nft(t, "delete", "rule", "inet", "sk", "web", "handle", strings.TrimPrefix(h, " # handle "))
		s := status(t)
		if s.Summary.Drift != 1 {
			t.Fatalf("status after the hand delete: %+v, want 1 drifted", s)
		}
		mustReconcile(t)
		if got := withoutHandles(listChain(t, "web")); strings.Join(got, "|") != strings.Join(webRules, "|") {
			t.Fatalf("web after reconcile = %q", got)
		}
		if s := status(t); s.Summary.Drift != 0 {
			t.Fatalf("status after reconcile: %+v", s)
		}
	})

	t.Run("a reordering is drift and is put back", func(t *testing.T) {
		nftScript(t, "flush chain inet sk ssh\nadd rule inet sk ssh drop\nadd rule inet sk ssh ip saddr 192.0.2.0/24 tcp dport 22 accept\n")
		if s := status(t); s.Summary.Drift != 1 {
			t.Fatalf("status after the reordering: %+v, want 1 drifted", s)
		}
		mustReconcile(t)
		if got := withoutHandles(listChain(t, "ssh")); strings.Join(got, "|") != strings.Join(sshRules, "|") {
			t.Fatalf("ssh after reconcile = %q", got)
		}
	})

	t.Run("a rule nft -c refuses changes nothing", func(t *testing.T) {
		before := nft(t, "-a", "list", "ruleset")
		writeIntent(t, chainRes("web", append([]string{"tcp dport 70000 accept"}, webRules...)...), chainRes("ssh", sshRules...))
		defer writeIntent(t, chainRes("web", webRules...), chainRes("ssh", sshRules...))
		// reconcile reports the failed Apply (and exits 0 with "not converged",
		// as for any provider).
		out, _ := sakutei(t, "reconcile")
		if !strings.Contains(out, "permanent: nftables: Apply: nft -c (check)") || !strings.Contains(out, "not converged") {
			t.Fatalf("reconcile with a bad rule did not report the refused check:\n%s", out)
		}
		if after := nft(t, "-a", "list", "ruleset"); after != before {
			t.Fatalf("the ruleset changed:\n--- before\n%s\n--- after\n%s", before, after)
		}
	})

	t.Run("declared as listed, no drift however many cycles", func(t *testing.T) {
		before := nft(t, "-a", "list", "ruleset")
		for i := 0; i < 5; i++ {
			mustReconcile(t)
			if s := status(t); s.Summary.Drift != 0 || s.Summary.Conformant != 2 || !s.Complete {
				t.Fatalf("cycle %d: %+v", i, s)
			}
		}
		// No Apply ran: the handles are the same (a re-applied chain gets new ones).
		if after := nft(t, "-a", "list", "ruleset"); after != before {
			t.Fatalf("the ruleset changed over the cycles:\n--- before\n%s\n--- after\n%s", before, after)
		}
	})

	t.Run("normal form", func(t *testing.T) {
		defer writeIntent(t, chainRes("web", webRules...), chainRes("ssh", sshRules...))
		// Whitespace and counter values only: not drift.
		writeIntent(t, chainRes("web", webRules...), chainRes("ssh", "ip saddr 192.0.2.0/24   tcp dport 22 accept", "drop"))
		if s := status(t); s.Summary.Drift != 0 {
			t.Fatalf("a whitespace-only difference is drift: %+v", s)
		}
		// A rule nft lists in another form: Apply reports nft's form instead of
		// leaving a drift that never goes away.
		writeIntent(t, chainRes("web", webRules...), chainRes("ssh", "ct state new,established accept", "drop"))
		out, _ := sakutei(t, "reconcile")
		if !strings.Contains(out, `as "ct state established,new accept"`) || !strings.Contains(out, "declare it in the form nft lists it") {
			t.Fatalf("reconcile did not report nft's form:\n%s", out)
		}
		writeIntent(t, chainRes("web", webRules...), chainRes("ssh", "ct state established,new accept", "drop"))
		if s := status(t); s.Summary.Drift != 0 {
			t.Fatalf("declared in nft's form, still drift: %+v", s)
		}
		writeIntent(t, chainRes("web", webRules...), chainRes("ssh", sshRules...))
		mustReconcile(t)
	})

	// Backlog #178: nft merges the adjacent ranges of an interval set when it
	// lists it back ({ 10.0.0.0/24, 10.0.1.0/24 } → { 10.0.0.0/23 }). The
	// comparison normalizes the sets, so it is neither a refused Apply nor drift.
	t.Run("a set nft merges is not drift", func(t *testing.T) {
		defer func() {
			writeIntent(t, chainRes("web", webRules...), chainRes("ssh", sshRules...))
			mustReconcile(t)
		}()
		merged := []string{
			"ip saddr { 10.0.0.0/24, 10.0.1.0/24 } tcp dport 22 accept",
			"ip6 saddr { 2001:db8:8000::/33, 2001:db8::/33 } tcp dport { 23, 22, 80-90, 91 } accept",
			"drop",
		}
		writeIntent(t, chainRes("web", webRules...), chainRes("ssh", merged...))
		if s := status(t); s.Summary.Drift != 1 {
			t.Fatalf("before: %+v, want 1 drifted", s)
		}
		out := mustReconcile(t)
		if strings.Contains(out, "permanent") || strings.Contains(out, "not converged") {
			t.Fatalf("reconcile did not converge:\n%s", out)
		}
		got := withoutHandles(listChain(t, "ssh"))
		want := []string{
			"ip saddr { 10.0.0.0/23 } tcp dport 22 accept",
			"ip6 saddr { 2001:db8::/32 } tcp dport { 22-23, 80-91 } accept",
			"drop",
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("nft did not merge the sets as expected: %q", got)
		}
		before := nft(t, "-a", "list", "ruleset")
		for i := 0; i < 3; i++ {
			if s := status(t); s.Summary.Drift != 0 || s.Summary.Conformant != 2 || !s.Complete {
				t.Fatalf("cycle %d: %+v, want conformant", i, s)
			}
			mustReconcile(t)
		}
		if after := nft(t, "-a", "list", "ruleset"); after != before {
			t.Fatalf("an Apply ran over the cycles:\n--- before\n%s\n--- after\n%s", before, after)
		}
		// A set that really differs is still drift.
		writeIntent(t, chainRes("web", webRules...), chainRes("ssh", append([]string{"ip saddr { 10.0.0.0/24, 10.0.2.0/24 } tcp dport 22 accept"}, merged[1:]...)...))
		if s := status(t); s.Summary.Drift != 1 {
			t.Fatalf("a different set: %+v, want 1 drifted", s)
		}
	})

	t.Run("the unmanaged table did not change", func(t *testing.T) {
		if got := nft(t, "list", "table", "ip", "other"); got != other {
			t.Fatalf("ip other changed:\n%s", got)
		}
	})
}
