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

package expand

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/intent"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

// ExecTimeout は外のコマンドの Expand の時間の上限。
const ExecTimeout = 30 * time.Second

// Exec は外のコマンドの Expand（§3.8 の exec:、E-A11・N-A14・E-A7）。stdin に入力の
// JSON を 1 つ渡し、stdout から出力の JSON を 1 つ読む。
//
// 入力：{"intent": {"<項目>": [{"doc", "line", "value"}…]}, "inputs": {…},
// "prior": {"resources": [{"type", "id", "ensure", "labels", "fields", "reason"}…]}}。
// intent は Claimed の項目だけ。
//
// 出力：intent と同じ形の resources:・ranges:・policies:・procedures: と、
// diagnostics: [{"severity": "reject|warn|unknown", "target", "message"}]。
// 終了コードが 0 でなければ誤り（その周は読み込めない）。同じ入力に同じ出力を
// 返すのはコマンドの責任（golden のテストで確かめる、N-P5）。
type Exec struct {
	Name    string
	Path    string
	Claimed []string
}

// Keys は intent の expand: の keys。
func (e *Exec) Keys() []string { return e.Claimed }

type execResource struct {
	Type   string            `json:"type"`
	ID     string            `json:"id"`
	Ensure string            `json:"ensure,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Fields core.Fields       `json:"fields,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

type execSection struct {
	Doc   string `json:"doc"`
	Line  int    `json:"line"`
	Value any    `json:"value"`
}

// Expand はコマンドを 1 回動かす。
func (e *Exec) Expand(ctx context.Context, in Input) (Output, error) {
	req := struct {
		Intent map[string][]execSection  `json:"intent"`
		Inputs map[string]ExternalInput  `json:"inputs"`
		Prior  map[string][]execResource `json:"prior"`
	}{Intent: map[string][]execSection{}, Inputs: in.Inputs, Prior: map[string][]execResource{"resources": {}}}
	for _, k := range e.Claimed {
		req.Intent[k] = []execSection{}
		for _, s := range Sections(in.Intent, k) {
			req.Intent[k] = append(req.Intent[k], execSection{Doc: s.Doc, Line: s.Line, Value: s.Value})
		}
	}
	if req.Inputs == nil {
		req.Inputs = map[string]ExternalInput{}
	}
	for _, r := range in.Prior.Resources {
		er := execResource{Type: r.ID.Type, ID: r.ID.Name, Labels: r.Labels, Fields: r.Fields, Reason: r.Reason}
		if r.Ensure == core.EnsureAbsent {
			er.Ensure = "absent"
		}
		req.Prior["resources"] = append(req.Prior["resources"], er)
	}
	stdin, err := json.Marshal(req)
	if err != nil {
		return Output{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Path)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		return Output{}, fmt.Errorf("外のコマンド %s: %v: %s", e.Path, err, msg)
	}
	return ParseExecOutput("exec:"+e.Name, stdout.Bytes())
}

// ParseExecOutput は外のコマンドの出力を読む。doc は診断に出す名前。
func ParseExecOutput(doc string, data []byte) (Output, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Output{}, fmt.Errorf("出力が JSON のオブジェクトでない: %w", err)
	}
	var out Output
	if d, ok := raw["diagnostics"]; ok {
		var ds []struct {
			Severity, Target, Message string
		}
		if err := json.Unmarshal(d, &ds); err != nil {
			return Output{}, fmt.Errorf("diagnostics が読めない: %w", err)
		}
		for _, x := range ds {
			sev := map[string]core.Severity{"reject": core.Reject, "warn": core.Warn, "unknown": core.Uncertain}
			s, ok := sev[x.Severity]
			if !ok || x.Message == "" {
				return Output{}, fmt.Errorf("diagnostics の severity は reject・warn・unknown で、message を書く")
			}
			out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: s, Origin: core.Origin{File: doc}, Target: x.Target, Message: x.Message})
		}
		delete(raw, "diagnostics")
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "resources" && k != "ranges" && k != "policies" && k != "procedures" {
			return Output{}, fmt.Errorf("出力の知らない項目 %q（書ける項目: resources, ranges, policies, procedures, diagnostics）", k)
		}
	}
	rest, _ := json.Marshal(raw)
	d, diags, err := DecodeJSON(doc, rest)
	if err != nil {
		return Output{}, err
	}
	out.Resources, out.Ranges, out.Policies, out.Procedures = d.Resources, d.Ranges, d.Policies, d.Procedures
	out.Diagnostics = append(out.Diagnostics, diags...)
	return out, nil
}

// DecodeJSON は intent と同じ形の JSON（resources:・ranges: など）を intent と同じ
// 読み方で読む（Expand のモジュールが作った宣言を Resource にする）。doc は診断に
// 出す名前。
func DecodeJSON(doc string, data []byte) (intent.Decoded, []core.Diagnostic, error) {
	n, err := yaml.ParseJSON(data)
	if err != nil {
		return intent.Decoded{}, nil, err
	}
	d, diags := intent.Decode(intent.Document{Path: doc, Root: n})
	return d, diags, nil
}
