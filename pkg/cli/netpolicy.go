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

package cli

// netpolicy.go — 網の間の許可（network_policy:、§7.6・Backlog #174）の展開の口
// と、plan・reconcile への出し方。
//
// 検査と展開は読み込み（loader）が行い、展開した resource は intent の
// resources と同じに収束する。ここは使う側が別の実現先（スイッチの ACL など）
// への変換を足す口（WithRealizer）と、展開と検査の結果を plan に出す所。

import (
	"fmt"
	"io"

	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
)

// WithRealizer は network_policy の実現先の変換を名前で足す（§7.6・#174）。
// target の realizer: にその名前を書いた実現先は、そこを通る許可（reason・
// tags を含む）を r に渡し、r が返した resource の宣言を収束させる。本体の
// nftables を含め、すでにある名前は起動のエラー（上書きしない）。
func WithRealizer(name string, r netpolicy.Realizer) Option {
	return func(o *options) { o.realizers = append(o.realizers, namedRealizer{name, r}) }
}

type namedRealizer struct {
	name string
	r    netpolicy.Realizer
}

// registerRealizers は Main の起動で WithRealizer の変換を足す。
func registerRealizers(rs []namedRealizer) error {
	for _, nr := range rs {
		if err := netpolicy.Register(nr.name, nr.r); err != nil {
			return err
		}
	}
	return nil
}

// netPolicyReport は検査と展開の結果を plan の report の形にする。
func netPolicyReport(res *netpolicy.Result) *planreport.NetworkPolicy {
	if res == nil {
		return nil
	}
	out := &planreport.NetworkPolicy{
		Networks: len(res.Policy.Networks), Allows: len(res.Policy.Allows),
		Findings: []planreport.NetworkPolicyFinding{}, Targets: []planreport.NetworkPolicyTarget{},
	}
	for _, f := range res.Findings {
		pf := planreport.NetworkPolicyFinding{Severity: f.Severity, Kind: f.Kind, Message: f.String()}
		if f.Allow != nil {
			pf.Allow, pf.Reason, pf.Tags = f.Allow.Ref, f.Allow.Reason, f.Allow.Tags
		}
		out.Findings = append(out.Findings, pf)
	}
	for _, z := range res.Realizations {
		t := planreport.NetworkPolicyTarget{Name: z.Target.Name, Realizer: z.Target.Realizer, Kind: z.Target.Kind,
			Resources: []string{}, Allows: []planreport.NetworkPolicyAllow{}}
		for _, r := range z.Output.Resources {
			t.Resources = append(t.Resources, r.Provider+"/"+r.ID)
		}
		for _, p := range z.Allows {
			a := p.Allow
			pa := planreport.NetworkPolicyAllow{Allow: a.Ref, From: a.From, To: a.To, Proto: a.Proto,
				Reason: a.Reason, Tags: a.Tags, Rules: []string{}}
			for _, pr := range a.Ports {
				pa.Ports = append(pa.Ports, pr.String())
			}
			for _, r := range z.Output.Rules {
				if r.Allow == a.Index {
					pa.Rules = append(pa.Rules, r.Text)
				}
			}
			t.Allows = append(t.Allows, pa)
		}
		out.Targets = append(out.Targets, t)
	}
	return out
}

// printNetPolicy は plan の文の出力に展開と検査の結果を出す。
func printNetPolicy(w io.Writer, np *planreport.NetworkPolicy) {
	if np == nil {
		return
	}
	fmt.Fprintf(w, "Network policy (§7.6): %d network(s), %d allow(s), %d target(s)\n", np.Networks, np.Allows, len(np.Targets))
	for _, f := range np.Findings {
		fmt.Fprintf(w, "  %s\n", f.Message)
	}
	for _, t := range np.Targets {
		fmt.Fprintf(w, "  target %s (%s, %s) → %d allow(s)", t.Name, t.Kind, t.Realizer, len(t.Allows))
		for _, r := range t.Resources {
			fmt.Fprintf(w, " %s", r)
		}
		fmt.Fprintln(w)
		for _, a := range t.Allows {
			ports := ""
			for i, p := range a.Ports {
				if i == 0 {
					ports = " "
				} else {
					ports += ","
				}
				ports += p
			}
			fmt.Fprintf(w, "    allow %s → %s %s%s (%s)\n", a.From, a.To, a.Proto, ports, a.Allow)
			reason := a.Reason
			if reason == "" {
				reason = "(none)"
			} else {
				reason = fmt.Sprintf("%q", reason)
			}
			fmt.Fprintf(w, "      reason: %s\n", reason)
			if len(a.Tags) > 0 {
				fmt.Fprintf(w, "      tags:   %v\n", a.Tags)
			}
			if len(a.Rules) == 0 {
				fmt.Fprintln(w, "      (no rule: no address family in common at this target)")
			}
			for _, r := range a.Rules {
				fmt.Fprintf(w, "      → %s\n", r)
			}
		}
	}
	fmt.Fprintln(w)
}

// warnNetPolicy は検査の warning を標準エラーに出す（reconcile・常駐）。
func warnNetPolicy(w io.Writer, commandName string, res *netpolicy.Result) {
	if res == nil {
		return
	}
	for _, f := range res.Findings {
		fmt.Fprintf(w, "%s: network_policy: %s\n", commandName, f.String())
	}
}
