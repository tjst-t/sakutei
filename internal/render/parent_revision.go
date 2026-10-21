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

// parent_revision.go tracks cause D per composite parent (#14, review D-1).
//
// Before #14 every child of every composite parent got UpstreamChanged=true
// whenever the global intent_revision / rendered_revision changed, so with two
// parents a change to one also tagged the other's children D.  Now each parent
// has its own rendered contribution hash (the rendered desired state of its
// children), and only the children of a parent whose hash changed since the
// previous cycle get D (§10.2).
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
)

// ParentRevisions returns composite parent Local ID → hash of that parent's
// rendered contribution: the parent's own rendered desired fields plus its
// children's IDs, desired fields and depends_on, in a deterministic order.
// Only parents with at least one child appear (the same set as
// BuildParentChildMap).
func ParentRevisions(expanded []ExpandedResource) map[string]string {
	parents := make(map[string]ExpandedResource)
	byParent := make(map[string][]ExpandedResource)
	for _, r := range expanded {
		switch {
		case r.IsComposite:
			parents[r.ID.Local] = r
		case r.ManagedBy != "":
			byParent[r.ManagedBy] = append(byParent[r.ManagedBy], r)
		}
	}
	result := make(map[string]string, len(byParent))
	for parent, children := range byParent {
		sort.Slice(children, func(i, j int) bool {
			return children[i].ID.String() < children[j].ID.String()
		})
		h := sha256.New()
		if p, ok := parents[parent]; ok {
			writeResource(h, "parent", p)
		}
		for _, c := range children {
			writeResource(h, "child", c)
		}
		result[parent] = hex.EncodeToString(h.Sum(nil))
	}
	return result
}

// writeResource writes r's ID, fields and depends_on to w, each value
// length-delimited so that values cannot alias each other.
func writeResource(w io.Writer, tag string, r ExpandedResource) {
	writeField(w, tag, r.ID.String())
	keys := make([]string, 0, len(r.Fields))
	for k := range r.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(w, "k", k)
		writeField(w, "v", r.Fields[k])
	}
	deps := append([]string(nil), r.DependsOn...)
	sort.Strings(deps)
	for _, d := range deps {
		writeField(w, "dep", d)
	}
}

func writeField(w io.Writer, tag, s string) {
	_, _ = fmt.Fprintf(w, "%s:%d:%s\n", tag, len(s), s)
}

// UpstreamChangedChildren returns the set of child Local IDs whose composite
// parent's rendered contribution changed between prev and cur (cause D).
//
// A nil prev means there is no previous cycle (cold start, or a cause-state
// written before #14): nothing is tagged D, the same as an empty previous
// revision.  A parent present in cur but not in a non-nil prev is new and
// counts as changed.
func UpstreamChangedChildren(prev, cur map[string]string, parentToChildren map[string][]string) map[string]bool {
	result := make(map[string]bool)
	if prev == nil {
		return result
	}
	for parent, childIDs := range parentToChildren {
		if p, ok := prev[parent]; ok && p == cur[parent] {
			continue
		}
		for _, childID := range childIDs {
			result[childID] = true
		}
	}
	return result
}
