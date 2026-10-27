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

package gitdist

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// DRDir は DR intent のプリステージ（shards/dr/<名前>.yaml）を置くディレクトリ（§25.1 (a)）。
// shards/ の下なので、全シャードの sparse checkout に必ず入る。
const DRDir = ShardsDir + "/dr"

// DRPlan は平時に PR 承認済みで保持する DR intent（§25.1 (a)）。
//
//	dr_plan:
//	  name: tokyo-to-osaka
//	  primary:            # 引き継がれる scope（平時の東京の claim）
//	    site: tokyo
//	  standby:            # 引き継ぐ側（記録用）
//	    site: osaka
//	  on_activation:
//	    claim_change:
//	      from:
//	        shard: tokyo      # 平時に primary を claim するシャード
//	      to:
//	        shard: osaka-dr   # 大阪で平時 idle で待つ DR シャード
//	    enabled: false        # 発動 = true（この 1 点の切替が DR 発動）
//
// claim は 1 シャード 1 scope なので、引き継ぐ側は standby 拠点で平時 idle の DR シャード
// （shards/osaka-dr.yaml が claim: 空）として待機する。enabled: true の間、from のシャードは
// idle になり、to のシャードが primary を claim する（claim ハンドオフ §17.7 と同じ経路）。
// 発動の判断は人がする（§25.4）: システムが enabled を書き換えることはない。
type DRPlan struct {
	Name    string
	Primary core.Scope
	Standby core.Scope
	From    string
	To      string
	Enabled bool
}

// ReadDRPlans は checkout の dir から shards/dr/ の DR plan をすべて読む（無ければ空）。
// 1 つでも読めない・矛盾する宣言があればエラー（曖昧な claim で収束しない）。
func ReadDRPlans(dir string) ([]DRPlan, error) {
	root := filepath.Join(dir, filepath.FromSlash(DRDir))
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gitdist: %w", err)
	}
	var plans []DRPlan
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml" && ext != ".json") {
			continue
		}
		rel := path.Join(DRDir, e.Name())
		tree, err := loader.ReadTree(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("gitdist: %s: %w", rel, err)
		}
		p, err := parseDRPlan(rel, strings.TrimSuffix(e.Name(), ext), tree)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Name < plans[j].Name })
	return plans, nil
}

func parseDRPlan(rel, stem string, tree map[string]any) (DRPlan, error) {
	bad := func(format string, a ...any) (DRPlan, error) {
		return DRPlan{}, fmt.Errorf("gitdist: %s: "+format, append([]any{rel}, a...)...)
	}
	if err := onlyKeys(tree, "dr_plan"); err != nil {
		return bad("%v", err)
	}
	m, ok := tree["dr_plan"].(map[string]any)
	if !ok {
		return bad("dr_plan must be a map")
	}
	if err := onlyKeys(m, "name", "primary", "standby", "on_activation"); err != nil {
		return bad("dr_plan: %v", err)
	}
	p := DRPlan{}
	p.Name, _ = m["name"].(string)
	if p.Name != stem {
		return bad("dr_plan.name %q must match the file name %q", p.Name, stem)
	}
	var err error
	if p.Primary, err = labelMap(m["primary"]); err != nil || len(p.Primary.Labels) == 0 {
		return bad("dr_plan.primary must be a non-empty map of scope labels")
	}
	if _, err := ScopeDir(p.Primary); err != nil {
		return bad("dr_plan.primary: %v", err)
	}
	if m["standby"] != nil {
		if p.Standby, err = labelMap(m["standby"]); err != nil {
			return bad("dr_plan.standby must be a map of labels")
		}
	}
	act, ok := m["on_activation"].(map[string]any)
	if !ok {
		return bad("dr_plan.on_activation is required")
	}
	if err := onlyKeys(act, "claim_change", "enabled"); err != nil {
		return bad("on_activation: %v", err)
	}
	enabled, ok := act["enabled"].(bool)
	if !ok {
		return bad("on_activation.enabled must be true or false (written explicitly)")
	}
	p.Enabled = enabled
	cc, ok := act["claim_change"].(map[string]any)
	if !ok {
		return bad("on_activation.claim_change is required")
	}
	if err := onlyKeys(cc, "from", "to"); err != nil {
		return bad("claim_change: %v", err)
	}
	for _, side := range []struct {
		key string
		dst *string
	}{{"from", &p.From}, {"to", &p.To}} {
		sm, ok := cc[side.key].(map[string]any)
		if !ok {
			return bad("claim_change.%s must be { shard: <name> }", side.key)
		}
		if err := onlyKeys(sm, "shard"); err != nil {
			return bad("claim_change.%s: %v", side.key, err)
		}
		name, _ := sm["shard"].(string)
		if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return bad("claim_change.%s.shard %q is not a shard name", side.key, name)
		}
		*side.dst = name
	}
	if p.From == p.To {
		return bad("claim_change.from and .to name the same shard %q", p.From)
	}
	return p, nil
}

// ResolveClaim は shards/<name>.yaml の claim に、有効な DR plan を効かせた claim を返す。
// plan は from/to のどちらかが name のとき、発動中の plan 名を返す（それ以外は ""）。
//
//   - 発動中の plan の from のシャード: idle になる。
//   - 発動中の plan の to のシャード: primary を claim する。
//
// 発動中の plan の宣言の矛盾（from の平時の claim が primary と一致しない・to が平時に idle で
// ない・1 つのシャードを 2 つ以上の発動中 plan が動かす）は、name に関わる plan かどうかに
// よらずエラーにする。片側のシャードだけが矛盾に気づくと、もう片側だけが plan に従って
// しまう（from だけが idle になり誰も primary を収束させない、など。試用 docs/trial/dr-mode
// で見つけた）。
func ResolveClaim(dir, name string) (sc ShardClaim, plan string, err error) {
	sc, err = ReadShardClaim(dir, name)
	if err != nil {
		return sc, "", err
	}
	plans, err := ReadDRPlans(dir)
	if err != nil {
		return sc, "", err
	}
	if err := checkActivePlans(dir, plans); err != nil {
		return sc, "", err
	}
	for _, p := range plans {
		if !p.Enabled || (p.From != name && p.To != name) {
			continue
		}
		plan = p.Name
		if p.From == name {
			sc.Claim, sc.Idle = core.Scope{}, true
		} else {
			sc.Claim, sc.Idle = p.Primary, false
		}
	}
	return sc, plan, nil
}

// checkActivePlans は発動中の plan の宣言を、両側のシャードの平時の claim と突き合わせる。
func checkActivePlans(dir string, plans []DRPlan) error {
	movedBy := map[string]string{}
	for _, p := range plans {
		if !p.Enabled {
			continue
		}
		for _, shard := range []string{p.From, p.To} {
			if other, ok := movedBy[shard]; ok {
				return fmt.Errorf("gitdist: shard %q is moved by two active DR plans (%s, %s)", shard, other, p.Name)
			}
			movedBy[shard] = p.Name
		}
		from, err := ReadShardClaim(dir, p.From)
		if err != nil {
			return fmt.Errorf("gitdist: DR plan %s: %w", p.Name, err)
		}
		if from.Idle || !from.Claim.Equal(p.Primary) {
			return fmt.Errorf("gitdist: DR plan %s: shard %q does not claim primary %s (claims %s)", p.Name, p.From, p.Primary, from.Claim)
		}
		to, err := ReadShardClaim(dir, p.To)
		if err != nil {
			return fmt.Errorf("gitdist: DR plan %s: %w", p.Name, err)
		}
		if !to.Idle {
			return fmt.Errorf("gitdist: DR plan %s: standby shard %q already claims %s; a DR shard must stand by idle", p.Name, p.To, to.Claim)
		}
	}
	return nil
}

func onlyKeys(m map[string]any, allowed ...string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok {
			return fmt.Errorf("unknown key %q", k)
		}
	}
	return nil
}

func labelMap(raw any) (core.Scope, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return core.Scope{}, errors.New("not a map")
	}
	labels := map[string]string{}
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		labels[k] = s
	}
	return core.NewScope(labels), nil
}
