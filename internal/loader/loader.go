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

// Package loader provides a minimal intent loader for Sprint 1 (Sead944-2).
//
// This loader reads JSON files from a directory and parses them into a list of
// DesiredResources understood by the diff engine.
//
// S33266e-2: この loader は JSON（機械経路）と YAML サブセット（人経路）の双方を受理し、
// ロード時に JSON Schema（intent_schema.go）で検証する。YAML と JSON は同一意味なら同一の
// DesiredResource 列を生む（フォーマット非依存）。YAML サブセットの境界は yaml.go を、
// 検証キーワードの境界は schema.go を参照。round-trip の canonical emit は
// emitCanonicalJSON（intent_schema.go）が担う。
//
// Intent file format (JSON, provisional):
//
//	{
//	  "resources": [
//	    {
//	      "provider": "fake",
//	      "scope": {"site": "tokyo"},   // optional; omit or {} for root scope
//	      "id": "my-resource",
//	      "depends_on": ["other-resource"],  // optional; S94f1f9-2 provisional
//	      // an entry may also be {"id": "vm-1", "ready_condition": "cloud_init_done"}
//	      // to wait for a named observable condition of the upstream (§8.3)
//	      "fields": {
//	        "name": "my-resource",
//	        "size": "10"
//	      }
//	    }
//	  ]
//	}
//
// All field values are strings in this provisional format.  The diff engine
// converts both desired and actual to strings for comparison.
//
// Scb0ecd-1: LoadDir now also computes and returns a core.RevisionInfo.
// See internal/revision for the computation details.
package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/internal/revision"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// parseDeletionPolicy converts the string form used in intent files to the
// core.DeletionPolicy enum.  Unknown strings map to ExplicitAbsentOnly (the
// safe default).
func parseDeletionPolicy(s string) core.DeletionPolicy {
	switch strings.TrimSpace(s) {
	case "PruneWithinScope":
		return core.PruneWithinScope
	default:
		// "ExplicitAbsentOnly" or empty string → safe default.
		return core.ExplicitAbsentOnly
	}
}

// treeToIntentFile はフォーマット非依存の in-memory 木（JSON Schema 検証済み）を
// intentFile へ変換する。scope/fields の値は provisional 形式に合わせて文字列化する
// （int/bool を含む YAML スカラーも決定的に文字列表現へ寄せ、JSON と等価にする）。
func treeToIntentFile(tree map[string]any) (intentFile, error) {
	var out intentFile
	rawResources, _ := tree["resources"].([]any)
	out.Resources = make([]intentResource, 0, len(rawResources))
	for i, raw := range rawResources {
		m, ok := raw.(map[string]any)
		if !ok {
			return intentFile{}, fmt.Errorf("resource[%d]: オブジェクトでなければなりません", i)
		}
		var r intentResource
		r.Provider, _ = m["provider"].(string)
		r.ID, _ = m["id"].(string)
		r.DeletionPolicy, _ = m["deletion_policy"].(string)
		r.ObserveTier, _ = m["observe_tier"].(string)
		r.Scope = stringifyStringMap(m["scope"])
		absent, err := parseEnsure(m)
		if err != nil {
			return intentFile{}, fmt.Errorf("resource[%d] (%v): %w", i, m["id"], err)
		}
		r.Absent = absent
		if ac, ok := m["apply_check"].(map[string]any); ok {
			parsed, err := parseApplyCheck(ac, "apply_check")
			if err != nil {
				return intentFile{}, fmt.Errorf("resource[%d] (%v): %w", i, m["id"], err)
			}
			if _, has := ac["provider"]; has {
				return intentFile{}, fmt.Errorf("resource[%d] (%v): apply_check: provider is for the top-level apply_check (the resource's provider is its own)", i, m["id"])
			}
			r.ApplyCheck = &parsed
		}
		if up, ok := m["update_policy"].(map[string]any); ok {
			pol, err := parseUpdatePolicy(up)
			if err != nil {
				return intentFile{}, fmt.Errorf("resource[%d] (%v): %w", i, m["id"], err)
			}
			r.UpdatePolicy = pol
		}
		fields, allocs, tmpls, err := splitFields(m["fields"])
		if err != nil {
			return intentFile{}, fmt.Errorf("resource[%d] (%v): %w", i, m["id"], err)
		}
		r.Fields, r.Allocations, r.AllocationTemplates = fields, allocs, tmpls
		if depRaw, ok := m["depends_on"].([]any); ok {
			for _, d := range depRaw {
				switch dep := d.(type) {
				case string:
					r.DependsOn = append(r.DependsOn, dep)
				case map[string]any:
					// { id, ready_condition }: wait for a named, observable
					// condition of the upstream, not just its processing (§8.3).
					id, _ := dep["id"].(string)
					r.DependsOn = append(r.DependsOn, id)
					if cond, _ := dep["ready_condition"].(string); cond != "" {
						if r.ReadyConditions == nil {
							r.ReadyConditions = make(map[string]string)
						}
						r.ReadyConditions[id] = cond
					}
				}
			}
		}
		out.Resources = append(out.Resources, r)
	}
	pools, err := parsePools(tree["pools"])
	if err != nil {
		return intentFile{}, err
	}
	out.Pools = pools
	scopes, err := parseScopes(tree["scopes"])
	if err != nil {
		return intentFile{}, err
	}
	out.Scopes = scopes
	tiers, err := parseObserveTiers(tree["observe_tiers"])
	if err != nil {
		return intentFile{}, err
	}
	out.ObserveTiers = tiers
	acs, err := parseApplyChecks(tree["apply_check"])
	if err != nil {
		return intentFile{}, err
	}
	out.ApplyChecks = acs
	rem, err := parseRemediation(tree["remediation"])
	if err != nil {
		return intentFile{}, err
	}
	out.Remediation = rem
	planned, err := parsePlannedChanges(tree["planned_change"])
	if err != nil {
		return intentFile{}, err
	}
	out.PlannedChanges = planned
	prof, err := parseProfile(tree)
	if err != nil {
		return intentFile{}, err
	}
	out.Profile = prof
	wfs, err := parseWorkflows(tree["workflows"])
	if err != nil {
		return intentFile{}, err
	}
	out.Workflows = wfs
	return out, nil
}

// splitFields separates plain field values from allocation requests.  A field
// whose value is the map { from_pool: <pool> } is an allocation request
// (§19.1: "VLAN が1つ必要" without naming the ID); the request may also carry
// template (the string the integer is written into, "192.0.2.{n}") and kind
// (ipv4 / ipv6, checked against the pool's range at load time), #145.  Any
// other map value is rejected rather than silently stringified.
func splitFields(v any) (fields, allocs map[string]string, tmpls map[string]diff.AllocationTemplate, err error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, nil, nil, nil
	}
	for k, val := range m {
		sub, isMap := val.(map[string]any)
		if !isMap {
			if fields == nil {
				fields = map[string]string{}
			}
			fields[k] = scalarToString(val)
			continue
		}
		pool, _ := sub["from_pool"].(string)
		if strings.TrimSpace(pool) == "" {
			return nil, nil, nil, fmt.Errorf("field %q: a map value must be { from_pool: <pool> } (allocation request, §19.1), optionally with template / kind", k)
		}
		for key := range sub {
			if key != "from_pool" && key != "template" && key != "kind" {
				return nil, nil, nil, fmt.Errorf("field %q: unknown key %q in the allocation request (from_pool / template / kind, §19.1・§19.8)", k, key)
			}
		}
		tmpl, err := parseAllocationTemplate(sub)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("field %q: %w", k, err)
		}
		if allocs == nil {
			allocs = map[string]string{}
		}
		allocs[k] = strings.TrimSpace(pool)
		if tmpl.Template != "" {
			if tmpls == nil {
				tmpls = map[string]diff.AllocationTemplate{}
			}
			tmpls[k] = tmpl
		}
	}
	return fields, allocs, tmpls, nil
}

// stringifyStringMap は map[string]any の各スカラー値を文字列へ寄せる。
// nil マップは nil を返す（呼び出し側で空マップ化される）。
func stringifyStringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = scalarToString(val)
	}
	return out
}

// scalarToString はスカラー値を決定的な文字列表現へ変換する。
func scalarToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// intentFile is the on-disk representation of a single intent file.
type intentFile struct {
	Resources    []intentResource `json:"resources"`
	Pools        []Pool           `json:"pools"`
	Scopes       []ScopePolicy    `json:"scopes"`
	ObserveTiers []ObserveTier    `json:"observe_tiers"`
	// ApplyChecks is the top-level apply_check (§11.1, Backlog #166).
	ApplyChecks []ProviderApplyCheck `json:"-"`
	// Remediation is the remediation schedule (§14.2); nil when undeclared.
	Remediation *remediation.Schedule `json:"-"`
	// PlannedChanges is the top-level planned_change (§13.3).
	PlannedChanges []plannedDecl `json:"-"`
	// Profile is the top-level profile: / solo: (§14.5).
	Profile profileDecl `json:"-"`
	// Workflows is the top-level workflows (§6.3).
	Workflows []workflow.Definition `json:"-"`
}

// intentResource is one resource declaration within an intent file.
type intentResource struct {
	// Provider is the provider name, e.g. "fake".
	Provider string `json:"provider"`
	// Scope is the label set for scope routing.  May be nil or empty.
	Scope map[string]string `json:"scope"`
	// ID is the provider-local identifier (Local part of composite ID).
	ID string `json:"id"`
	// DependsOn is the list of provider-local IDs (same intent file scope)
	// that this resource depends on.  Provisional format; S33266e formalises.
	DependsOn []string `json:"depends_on"`
	// ReadyConditions maps a DependsOn entry to the ready condition declared
	// with it (`depends_on: [{id, ready_condition}]`, §8.3).
	ReadyConditions map[string]string `json:"-"`
	// Fields is the map of field name → desired value (string).
	Fields map[string]string `json:"fields"`
	// DeletionPolicy controls how the engine handles resources absent from
	// the intent.  "ExplicitAbsentOnly" (default) or "PruneWithinScope".
	// Story Scf7762-1.
	//
	// NOTE: there is deliberately no "pruneable" field here.  As of Scf7762-1,
	// pruneability is a TYPE-level property declared by the provider via
	// Capabilities().Pruneable (ADR-0005), not a per-instance intent flag.  A
	// legacy "pruneable" key in an intent file is silently ignored (provisional
	// format; S33266e will formalise the schema and reject unknown keys).
	DeletionPolicy string `json:"deletion_policy"`

	// ObserveTier is the Observe-frequency tier (§16.5).  Empty means every
	// cycle.  Deliberately not `tier:`, which §8 reserves for the DAG priority
	// band (`tier: foundation`).
	ObserveTier string `json:"observe_tier"`

	// Allocations maps a field to the pool it is allocated from — the
	// `field: { from_pool: <pool> }` request form (§19.1).
	Allocations map[string]string `json:"-"`
	// AllocationTemplates maps an allocated field to its template / kind
	// (#145).
	AllocationTemplates map[string]diff.AllocationTemplate `json:"-"`

	// UpdatePolicy is the update_policy (§6.2): strategy / window /
	// rollback_on_fail.
	UpdatePolicy core.UpdatePolicy `json:"-"`

	// Absent is `ensure: absent` (§12.2, Backlog #163).
	Absent bool `json:"-"`

	// ApplyCheck is the resource's `apply_check:` (§11.1, Backlog #166).
	ApplyCheck *diff.ApplyCheck `json:"-"`
}

// absentKeys are the only keys a resource declared `ensure: absent` may carry:
// its identity, and how often it is observed.
var absentKeys = map[string]bool{"provider": true, "scope": true, "id": true, "ensure": true, "observe_tier": true, "apply_check": true}

// parseEnsure reads `ensure:` (§12.2, Backlog #163): present (the default) or
// absent.  An absent resource declares nothing else but its identity and
// observe_tier: fields, depends_on, update_policy and deletion_policy are
// refused rather than ignored (a field would read as a state the resource is
// kept in; a deletion_policy as a scope it closes).
func parseEnsure(m map[string]any) (bool, error) {
	e, _ := m["ensure"].(string)
	switch strings.TrimSpace(e) {
	case "", "present":
		return false, nil
	case "absent":
	default:
		return false, fmt.Errorf("ensure %q: want present or absent", e)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !absentKeys[k] {
			return false, fmt.Errorf("ensure: absent cannot be declared with %s (an absent resource declares only provider, scope, id and observe_tier)", k)
		}
	}
	return true, nil
}

// parseUpdatePolicy reads update_policy (§6.2).  window and rollback_on_fail
// only mean something for strategy: task; declaring them on the Apply strategy
// is rejected rather than silently ignored (a window that does not gate
// anything would read as a safety that is not there).
func parseUpdatePolicy(m map[string]any) (core.UpdatePolicy, error) {
	var pol core.UpdatePolicy
	strategy, _ := m["strategy"].(string)
	if strategy == "task" {
		pol.Strategy = core.UpdateStrategyTask
	}
	pol.Window, _ = m["window"].(string)
	pol.Window = strings.TrimSpace(pol.Window)
	pol.RollbackOnFail, _ = m["rollback_on_fail"].(bool)
	if !pol.IsTask() && (pol.Window != "" || pol.RollbackOnFail) {
		return core.UpdatePolicy{}, fmt.Errorf("update_policy: window / rollback_on_fail require strategy: task")
	}
	if _, err := task.ParseWindow(pol.Window); err != nil {
		return core.UpdatePolicy{}, fmt.Errorf("update_policy: %w", err)
	}
	return pol, nil
}

// LoadDir reads all JSON files in dir and returns the desired resources and
// revision information.
//
// Files are processed in lexicographic order for deterministic output.
// Non-JSON files (by extension) are silently skipped.
// Subdirectories are not traversed.
//
// S94f1f9-2: cycle detection is performed after all files are loaded.
// If depends_on forms a cycle among the loaded resources, LoadDir returns an
// explicit error and the caller must not begin convergence.  This implements
// the "循環検出は理想状態ロード時に一度" contract (§8.1).
//
// Scb0ecd-1: LoadDir now also computes core.RevisionInfo:
//   - intent_revision: SHA-256 of all intent file contents (sorted by name).
//   - rendered_revision: equal to intent_revision in v1a (seam for S072a14).
//
// The revision is computed over the root scope (empty Scope) because the
// provisional loader does not yet support multi-scope intent directories.
// Multi-scope revision computation is deferred to S33266e.
func LoadDir(dir string) ([]diff.DesiredResource, core.RevisionInfo, error) {
	in, err := LoadIntent(dir)
	if err != nil {
		return nil, core.RevisionInfo{}, err
	}
	return in.Resources, in.Revision, nil
}

// Intent is everything an intent directory declares: resources plus the
// top-level declarations (pools for allocation, §19.8).
type Intent struct {
	Resources    []diff.DesiredResource
	Pools        []Pool
	Scopes       []ScopePolicy
	ObserveTiers []ObserveTier
	// ApplyChecks are the top-level apply_check declarations: the settle
	// and the stop limits per provider type (§11.1, Backlog #166).
	ApplyChecks []ProviderApplyCheck
	// Remediation is the remediation schedule (§14.2), declared in at most
	// one intent file.  Nil when no file declares remediation:.
	Remediation *remediation.Schedule
	// PlannedChanges are the planned_change declarations (§13.3), each with
	// its gate resolved from the operational profile (§14.5).
	PlannedChanges []budget.PlannedChange
	// Profile is the operational profile (§14.5); team when undeclared.
	Profile Profile
	// Workflows are the minimal Workflow declarations (§6.3).
	Workflows []workflow.Definition
	Revision  core.RevisionInfo
}

// LoadIntent reads every intent file in dir (same rules as LoadDir) and also
// returns the top-level declarations.  It validates that pool names are unique
// and that every allocation request names a declared pool.
func LoadIntent(dir string) (Intent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Intent{}, fmt.Errorf("loader: read intent dir %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if fileFormat(entry.Name()) == "" {
			continue // .json/.yaml/.yml 以外は黙ってスキップ
		}
		names = append(names, entry.Name())
	}
	return loadIntentFiles(dir, names, revision.Compute)
}

// LoadIntentTree は LoadIntent と同じ規則で、dir 配下のサブディレクトリも含めて
// 全 intent ファイルを読む。git 配布のレイアウト intent/<scope>/...（§17.4）用で、
// sparse checkout で手元に残るのは自 claim 分と共有 platform 分だけになる。
// dir が無いときはエラーにする（無い木を「何も望まない」として描かない、§9.2）。
func LoadIntentTree(dir string) (Intent, error) {
	rels, err := revision.IntentTreeFiles(dir)
	if err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	return loadIntentFiles(dir, rels, revision.ComputeTree)
}

// loadIntentFiles reads the given intent files (slash-separated, relative to
// dir), validates the whole, and computes the revision with rev.
func loadIntentFiles(dir string, names []string, rev func(string, core.Scope) (core.RevisionInfo, error)) (Intent, error) {
	var resources []diff.DesiredResource
	var pools []Pool
	var scopes []ScopePolicy
	var tiers []ObserveTier
	var applyChecks []ProviderApplyCheck
	var rem *remediation.Schedule
	remFrom := ""
	var decls []plannedDecl
	var profiles []profileDecl
	var workflows []workflow.Definition
	for _, name := range names {
		path := filepath.Join(dir, filepath.FromSlash(name))
		loaded, file, err := loadFile(path)
		if err != nil {
			return Intent{}, fmt.Errorf("loader: %w", err)
		}
		resources = append(resources, loaded...)
		pools = append(pools, file.Pools...)
		scopes = append(scopes, file.Scopes...)
		tiers = append(tiers, file.ObserveTiers...)
		applyChecks = append(applyChecks, file.ApplyChecks...)
		if file.Remediation != nil {
			if rem != nil {
				return Intent{}, fmt.Errorf("loader: remediation: declared in both %s and %s; declare the schedule once (§14.2)", remFrom, name)
			}
			rem, remFrom = file.Remediation, name
		}
		decls = append(decls, file.PlannedChanges...)
		profiles = append(profiles, file.Profile)
		workflows = append(workflows, file.Workflows...)
	}

	// D-2: reject duplicate Local IDs.  Two resources sharing a Local ID would
	// otherwise be indistinguishable to Observe/Diff/Apply (which key on the
	// Local ID) and to depends_on resolution, causing one to silently shadow the
	// other.  Fail loudly instead of picking a winner.
	if err := detectDuplicateLocalIDs(resources); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}

	// S94f1f9-2: reject cyclic depends_on graphs at load time.
	if err := detectCycles(resources); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}

	// §12.2 (#163): nothing depends on an explicit absent.
	if err := diff.ValidateAbsent(resources); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}

	if err := validatePools(pools, resources); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	if err := validateScopes(scopes, resources); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	if err := validateObserveTiers(tiers, resources); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	if err := validateApplyChecks(applyChecks); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	prof, err := mergeProfiles(profiles, names)
	if err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	planned := make([]budget.PlannedChange, 0, len(decls))
	for _, d := range decls {
		pc := d.pc
		pc.Gate = resolvePlannedGate(prof, d)
		planned = append(planned, pc)
	}
	if err := validatePlannedChanges(planned); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	if err := applyProfileToWorkflows(prof, workflows); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}
	if err := validateWorkflows(workflows, resources); err != nil {
		return Intent{}, fmt.Errorf("loader: %w", err)
	}

	// Scb0ecd-1: compute revision info after all validation passes so that only
	// valid intent contributes to the hash.  Use the root scope (empty labels)
	// for the provisional single-scope loader.
	rootScope := core.NewScope(nil)
	ri, err := rev(dir, rootScope)
	if err != nil {
		return Intent{}, fmt.Errorf("loader: revision: %w", err)
	}

	return Intent{Resources: resources, Pools: pools, Scopes: scopes, ObserveTiers: tiers, ApplyChecks: applyChecks, Remediation: rem, PlannedChanges: planned, Profile: prof.profile, Workflows: workflows, Revision: ri}, nil
}

// detectDuplicateLocalIDs returns an error if two loaded resources share the
// same Local ID (D-2).  The first duplicate encountered is named in the error.
func detectDuplicateLocalIDs(resources []diff.DesiredResource) error {
	seen := make(map[string]struct{}, len(resources))
	for _, r := range resources {
		local := r.ID.Local
		if _, dup := seen[local]; dup {
			return fmt.Errorf("duplicate resource id %q (declared more than once)", local)
		}
		seen[local] = struct{}{}
	}
	return nil
}

// detectCycles runs Kahn's algorithm over the depends_on graph.  If any cycle
// exists (i.e. not all nodes can be topologically sorted), it returns an error
// naming the involved IDs.
//
// Only Local IDs are used here because depends_on names are local within the
// same intent scope (§8.1, §21).
func detectCycles(resources []diff.DesiredResource) error {
	// Build ID → index map.
	idx := make(map[string]int, len(resources))
	for i, r := range resources {
		idx[r.ID.Local] = i
	}

	// Build in-degree and adjacency list.
	inDeg := make([]int, len(resources))
	adj := make([][]int, len(resources))
	for i, r := range resources {
		for _, dep := range r.DependsOn {
			j, ok := idx[dep]
			if !ok {
				// depends_on references an unknown ID — let the DAG builder
				// catch this at apply time; cycle detection only needs the
				// graph structure.
				continue
			}
			// Edge: dep → r (r depends on dep; dep must come before r).
			adj[j] = append(adj[j], i)
			inDeg[i]++
		}
	}

	// Kahn's algorithm: enqueue all in-degree-0 nodes.
	queue := make([]int, 0, len(resources))
	for i, d := range inDeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	processed := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		processed++
		for _, m := range adj[n] {
			inDeg[m]--
			if inDeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}

	if processed < len(resources) {
		// Collect IDs that are part of the cycle (still have in-degree > 0).
		var cyclic []string
		for i, d := range inDeg {
			if d > 0 {
				cyclic = append(cyclic, resources[i].ID.Local)
			}
		}
		return fmt.Errorf("cyclic dependency detected among resources: %s", strings.Join(cyclic, ", "))
	}
	return nil
}

// ReadTree は JSON / YAML（intent と同じサブセット）のファイルを、フォーマット
// 非依存の木として読む。intent 以外の宣言ファイル（例: シャード割当 shards/<名前>.yaml、
// §17.1）を同じ書式で読むために使う。
func ReadTree(path string) (map[string]any, error) {
	format := fileFormat(path)
	if format == "" {
		return nil, fmt.Errorf("loader: %s: 拡張子が .json / .yaml / .yml でない", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loader: %w", err)
	}
	tree, err := decodeIntentTree(data, format)
	if err != nil {
		return nil, fmt.Errorf("loader: %s: %w", path, err)
	}
	return tree, nil
}

// fileFormat は拡張子から intent フォーマット（"json"/"yaml"）を判定する。
// 対象外の拡張子は "" を返す（呼び出し側でスキップ）。
func fileFormat(name string) string {
	switch {
	case strings.HasSuffix(name, ".json"):
		return "json"
	case strings.HasSuffix(name, ".yaml"), strings.HasSuffix(name, ".yml"):
		return "yaml"
	default:
		return ""
	}
}

// loadFile parses a single intent file (JSON or YAML subset) and returns its
// resources.  The pipeline is: バイト列 → decodeIntentTree（フォーマット非依存の木）
// → JSON Schema 検証 → intentFile へデコード → DesiredResource。
func loadFile(path string) ([]diff.DesiredResource, intentFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, intentFile{}, fmt.Errorf("read intent file %s: %w", path, err)
	}

	format := fileFormat(filepath.Base(path))
	tree, err := decodeIntentTree(data, format)
	if err != nil {
		return nil, intentFile{}, fmt.Errorf("parse intent file %s: %w", path, err)
	}

	// S33266e-2: ロード時に JSON Schema で検証する（未知キー・型曖昧性・enum を強制）。
	// 不正 intent はここで明確なエラーで拒否し、収束を開始させない。
	if err := validateAgainstSchema(tree, intentSchema()); err != nil {
		return nil, intentFile{}, fmt.Errorf("intent file %s: %w", path, err)
	}

	intent, err := treeToIntentFile(tree)
	if err != nil {
		return nil, intentFile{}, fmt.Errorf("intent file %s: %w", path, err)
	}

	resources := make([]diff.DesiredResource, 0, len(intent.Resources))
	for i, r := range intent.Resources {
		if r.Provider == "" {
			return nil, intentFile{}, fmt.Errorf("intent file %s: resource[%d]: missing provider", path, i)
		}
		if r.ID == "" {
			return nil, intentFile{}, fmt.Errorf("intent file %s: resource[%d]: missing id", path, i)
		}

		scope := core.NewScope(r.Scope) // handles nil → empty scope
		id := core.NewID(r.Provider, scope, r.ID)

		fields := r.Fields
		if fields == nil {
			fields = map[string]string{}
		}

		dependsOn := r.DependsOn
		if dependsOn == nil {
			dependsOn = []string{}
		}

		resources = append(resources, diff.DesiredResource{
			ID:                  id,
			Fields:              fields,
			DependsOn:           dependsOn,
			ReadyConditions:     r.ReadyConditions,
			DeletionPolicy:      parseDeletionPolicy(r.DeletionPolicy),
			Allocations:         r.Allocations,
			AllocationTemplates: r.AllocationTemplates,
			ObserveTier:         strings.TrimSpace(r.ObserveTier),
			UpdatePolicy:        r.UpdatePolicy,
			Absent:              r.Absent,
			ApplyCheck:          r.ApplyCheck,
		})
	}
	return resources, intent, nil
}

// LoadResourcesFile reads one intent-format file (.json/.yaml/.yml) and returns
// its resources.  Render phase 1 uses it to read the resources an external
// inventory (CMDB) composes (§9.1 inventory 合成).  An inventory carries
// resources only: a file that also declares pools or scopes is rejected
// rather than having those declarations silently ignored.
func LoadResourcesFile(path string) ([]diff.DesiredResource, error) {
	if fileFormat(filepath.Base(path)) == "" {
		return nil, fmt.Errorf("loader: %s: extension must be .json, .yaml or .yml", path)
	}
	resources, file, err := loadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loader: %w", err)
	}
	if len(file.Pools) > 0 || len(file.Scopes) > 0 {
		return nil, fmt.Errorf("loader: %s: an inventory file declares resources only (pools/scopes belong in intent)", path)
	}
	if err := detectDuplicateLocalIDs(resources); err != nil {
		return nil, fmt.Errorf("loader: %s: %w", path, err)
	}
	return resources, nil
}
