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
	"strings"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/revision"
	"github.com/tjst-t/sakutei/pkg/core"
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
		r.Scope = stringifyStringMap(m["scope"])
		r.Fields = stringifyStringMap(m["fields"])
		if depRaw, ok := m["depends_on"].([]any); ok {
			for _, d := range depRaw {
				if s, ok := d.(string); ok {
					r.DependsOn = append(r.DependsOn, s)
				}
			}
		}
		out.Resources = append(out.Resources, r)
	}
	return out, nil
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
	Resources []intentResource `json:"resources"`
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, core.RevisionInfo{}, fmt.Errorf("loader: read intent dir %s: %w", dir, err)
	}

	var resources []diff.DesiredResource
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if fileFormat(name) == "" {
			continue // .json/.yaml/.yml 以外は黙ってスキップ
		}

		path := filepath.Join(dir, name)
		loaded, err := loadFile(path)
		if err != nil {
			return nil, core.RevisionInfo{}, fmt.Errorf("loader: %w", err)
		}
		resources = append(resources, loaded...)
	}

	// D-2: reject duplicate Local IDs.  Two resources sharing a Local ID would
	// otherwise be indistinguishable to Observe/Diff/Apply (which key on the
	// Local ID) and to depends_on resolution, causing one to silently shadow the
	// other.  Fail loudly instead of picking a winner.
	//
	// NOTE: Sprint 1 keys purely on Local ID (Scope is empty for the fake).
	// When multi-Scope support lands (S33266e), extend this check to the full
	// composite ID so that the same Local ID under different Scopes is allowed.
	if err := detectDuplicateLocalIDs(resources); err != nil {
		return nil, core.RevisionInfo{}, fmt.Errorf("loader: %w", err)
	}

	// S94f1f9-2: reject cyclic depends_on graphs at load time.
	if err := detectCycles(resources); err != nil {
		return nil, core.RevisionInfo{}, fmt.Errorf("loader: %w", err)
	}

	// Scb0ecd-1: compute revision info after all validation passes so that only
	// valid intent contributes to the hash.  Use the root scope (empty labels)
	// for the provisional single-scope loader.
	rootScope := core.NewScope(nil)
	rev, err := revision.Compute(dir, rootScope)
	if err != nil {
		return nil, core.RevisionInfo{}, fmt.Errorf("loader: revision: %w", err)
	}

	return resources, rev, nil
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
func loadFile(path string) ([]diff.DesiredResource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read intent file %s: %w", path, err)
	}

	format := fileFormat(filepath.Base(path))
	tree, err := decodeIntentTree(data, format)
	if err != nil {
		return nil, fmt.Errorf("parse intent file %s: %w", path, err)
	}

	// S33266e-2: ロード時に JSON Schema で検証する（未知キー・型曖昧性・enum を強制）。
	// 不正 intent はここで明確なエラーで拒否し、収束を開始させない。
	if err := validateAgainstSchema(tree, intentSchema()); err != nil {
		return nil, fmt.Errorf("intent file %s: %w", path, err)
	}

	intent, err := treeToIntentFile(tree)
	if err != nil {
		return nil, fmt.Errorf("intent file %s: %w", path, err)
	}

	resources := make([]diff.DesiredResource, 0, len(intent.Resources))
	for i, r := range intent.Resources {
		if r.Provider == "" {
			return nil, fmt.Errorf("intent file %s: resource[%d]: missing provider", path, i)
		}
		if r.ID == "" {
			return nil, fmt.Errorf("intent file %s: resource[%d]: missing id", path, i)
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
			ID:             id,
			Fields:         fields,
			DependsOn:      dependsOn,
			DeletionPolicy: parseDeletionPolicy(r.DeletionPolicy),
		})
	}
	return resources, nil
}
