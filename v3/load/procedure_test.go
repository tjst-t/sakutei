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

package load_test

import (
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

const procConfig = baseConfig + `  bmc:
    module: fake
    settings:
      kinds:
        system:
          fields: {firmware: string, power: string}
          ops:
            apply_update: {destructive: true}
            reset: {}
`

const bmcFW = `
resources:
  - {type: bmc/system, id: n1, labels: {rack: "3"}, fields: {firmware: "2.1", power: "On"}}
procedures:
  - name: bmc-fw
    target: {type: bmc/system, select: {labels: {rack: "3"}}}
    trigger: on_drift
    drift_fields: [firmware]
    concurrency: 2
    timeout: 6h
    on_failure: halt
    invariant: 'count(bmc/system[rack="3"], it.power == "On") >= 1'
    steps:
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [bob]}}, timeout: 24h}
      - name: apply
        critical: true
        invoke: {op: apply_update, args: {version: "{desired.firmware}"}}
      - name: confirm
        wait: {when: 'observe(bmc/system/{target}).firmware == desired.firmware', timeout: 30m}
    done_when: 'observe(bmc/system/{target}).firmware == desired.firmware'
`

// Procedure を intent から読む（§3.7）。
func TestLoadProcedures(t *testing.T) {
	l, err := run(t, procConfig, map[string]string{"p.yaml": bmcFW})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Procedures) != 1 {
		t.Fatalf("%+v", l.Procedures)
	}
	p := l.Procedures[0]
	if p.Concurrency != 2 || p.Timeout != 6*time.Hour || p.Trigger.Kind != core.TriggerOnDrift || len(p.Steps) != 3 ||
		p.Steps[0].Gate.Timeout != 24*time.Hour || !p.Steps[1].Critical || p.Steps[1].Args["version"] != "{desired.firmware}" ||
		p.Steps[2].Timeout != 30*time.Minute {
		t.Fatalf("%+v", p)
	}
}

func TestLoadRejectsProcedures(t *testing.T) {
	head := "resources:\n  - {type: bmc/system, id: n1, labels: {rack: \"3\"}, fields: {firmware: \"2.1\"}}\nprocedures:\n"
	cases := []struct {
		name, body string
		wants      []string
	}{
		{"知らない操作", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{invoke: {op: flash}}]}\n",
			[]string{`操作 "flash" が無い`, "apply_update, reset"}},
		{"操作の無い type", "  - {name: a, target: {type: dns/record, id: www}, trigger: manual, steps: [{invoke: {op: x}}]}\n",
			[]string{`type "dns/record" に操作 "x" が無い`}},
		{"drift_fields が無い field", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: on_drift, drift_fields: [bios], steps: [{invoke: {op: reset}}]}\n",
			[]string{`drift_fields の "bios"`}},
		{"on_drift に drift_fields が無い", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: on_drift, steps: [{invoke: {op: reset}}]}\n",
			[]string{"drift_fields を書く"}},
		{"manual に done_when が無い", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{manual: {instruction: x}}]}\n",
			[]string{"done_when を書く"}},
		{"step の kind が 2 つ", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{invoke: {op: reset}, wait: {for: 1m}}]}\n",
			[]string{"どれか 1 つ"}},
		{"wait に when と for", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{wait: {for: 1m, when: 'true'}}]}\n",
			[]string{"when か for のどちらか"}},
		{"式の誤り", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, done_when: 'observe(bmc/system/{target}).bios == 1', steps: [{wait: {for: 1m}}]}\n",
			[]string{`"bios" が無い`}},
		{"critical の wait", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{critical: true, wait: {for: 1m}}]}\n",
			[]string{"critical は apply・invoke"}},
		{"separation の gate に申請者が無い", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: on_drift, drift_fields: [firmware], steps: [{gate: {approval: {quorum: {n: 1, of: [bob]}, separation: true}}}]}\n",
			[]string{"trigger: manual の Procedure にだけ"}},
		{"quorum が満たせない", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{gate: {approval: {quorum: {n: 2, of: [bob]}}}}]}\n",
			[]string{"n 2 が of の 1 人より多い"}},
		{"同じ field を 2 つの Procedure", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: on_drift, drift_fields: [firmware], steps: [{invoke: {op: reset}}]}\n  - {name: b, target: {type: bmc/system, select: {labels: {rack: \"3\"}}}, trigger: on_drift, drift_fields: [firmware], steps: [{invoke: {op: reset}}]}\n",
			[]string{`"firmware" を Procedure a も受け持つ`}},
		{"同じ名前", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{wait: {for: 1m}}]}\n  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{wait: {for: 1m}}]}\n",
			[]string{"同じ名前の Procedure"}},
		{"rollback は後の Stage", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, on_failure: rollback, steps: [{wait: {for: 1m}}]}\n",
			[]string{"on_failure は halt だけ"}},
		{"step の policy は report だけ", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{policy: {select: {all: true}, mode: apply}}]}\n",
			[]string{"mode: report だけ"}},
		{"apply の知らない field", "  - {name: a, target: {type: bmc/system, id: n1}, trigger: manual, steps: [{apply: {type: bmc/system, id: '{target}', fields: {bios: x}}}]}\n",
			[]string{`field "bios" が無い`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, procConfig, map[string]string{"p.yaml": head + tc.body})
			expectReject(t, err, tc.wants...)
		})
	}
}

// Policy の approval と applicant（§3.6、E-L2・N-L2）。
func TestLoadPolicyApproval(t *testing.T) {
	l, err := run(t, baseConfig, map[string]string{"p.yaml": `
policies:
  - name: platform
    select: {labels: {owner: platform}}
    applicant: carol
    approval: {quorum: {n: 2, of: [alice, bob, carol]}, separation: true, timeout: 24h}
`})
	if err != nil {
		t.Fatal(err)
	}
	a := l.Policies[0].Approval
	if a == nil || a.N != 2 || len(a.Of) != 3 || !a.Separation || a.Timeout != 24*time.Hour || l.Policies[0].Applicant != "carol" {
		t.Fatalf("%+v", l.Policies[0])
	}
	for name, tc := range map[string]struct{ body, want string }{
		"申請者が無い separation":      {"policies:\n  - {name: a, select: {all: true}, approval: {quorum: {n: 1, of: [bob]}, separation: true}}\n", "applicant（申請者）を書く"},
		"approval の無い applicant": {"policies:\n  - {name: a, select: {all: true}, applicant: carol}\n", "applicant は approval と一緒に"},
		"of が無い":                 {"policies:\n  - {name: a, select: {all: true}, approval: {quorum: {n: 1}}}\n", "of（承認者の一覧）を書く"},
		"同じ承認者":                  {"policies:\n  - {name: a, select: {all: true}, approval: {quorum: {n: 1, of: [bob, bob]}}}\n", `承認者 "bob" が 2 回`},
		"n が 0":                  {"policies:\n  - {name: a, select: {all: true}, approval: {quorum: {n: 0, of: [bob]}}}\n", "1 以上の整数"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(t, baseConfig, map[string]string{"p.yaml": tc.body})
			expectReject(t, err, tc.want)
		})
	}
}
