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

package loader

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AllocationFileSchemaVersion is the schema_version of the allocation file
// this build reads and writes (docs/allocation-file.md).
const AllocationFileSchemaVersion = 1

// AllocationFileHeader is the first line of an allocation file written by
// Sakutei.  It is a YAML comment: the file stays plain YAML.
const AllocationFileHeader = "# sakutei:generated allocation-write-back"

// AllocationFile is what a pool with `write_back: true` handed out, written
// back to the intent side (#157, §19.7): the source of truth for the pool's
// allocations.  The form is the tool's own business no more than the intent
// is — plain YAML, nothing of the store's internal JSON:
//
//	schema_version: 1
//	pool: vm-ids
//	allocations:
//	  - owner: "proxmox-vm/web-1#vmid"
//	    value: 101
//	    allocated_at: "2026-10-28T09:00:00Z"
type AllocationFile struct {
	SchemaVersion int
	Pool          string
	Allocations   []AllocationEntry
}

// AllocationEntry is one allocation: owner holds value since allocated_at
// (the time it was written back).
type AllocationEntry struct {
	Owner       string
	Value       int
	AllocatedAt time.Time
}

// ByOwner returns the entries keyed by owner.
func (f AllocationFile) ByOwner() map[string]AllocationEntry {
	out := make(map[string]AllocationEntry, len(f.Allocations))
	for _, e := range f.Allocations {
		out[e.Owner] = e
	}
	return out
}

// ReadAllocationFile reads and checks the allocation file of pool at path.
// exists is false (and err nil) when there is no file.  A file of another
// schema_version or another pool, a duplicate owner or value, or a value
// outside the integers is an error: the file is the source of truth, so a
// file that cannot be read for sure holds the pool back rather than being
// guessed at.
func ReadAllocationFile(path, pool string) (f AllocationFile, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return AllocationFile{SchemaVersion: AllocationFileSchemaVersion, Pool: pool}, false, nil
	}
	if err != nil {
		return AllocationFile{}, true, fmt.Errorf("loader: %w", err)
	}
	f, err = ParseAllocationFile(data, path, pool)
	return f, true, err
}

// ParseAllocationFile is ReadAllocationFile on the bytes of a file (name is
// used in errors only; its extension picks JSON or YAML, YAML when neither).
func ParseAllocationFile(data []byte, path, pool string) (f AllocationFile, err error) {
	format := fileFormat(path)
	if format == "" {
		format = "yaml"
	}
	tree, err := decodeIntentTree(data, format)
	if err != nil {
		return AllocationFile{}, fmt.Errorf("loader: %s: %w", path, err)
	}
	for k := range tree {
		switch k {
		case "schema_version", "pool", "allocations":
		default:
			return AllocationFile{}, fmt.Errorf("%s: unknown key %q (schema_version, pool, allocations)", path, k)
		}
	}
	ver, ok := asInt(tree["schema_version"])
	if !ok {
		return AllocationFile{}, fmt.Errorf("%s: schema_version is missing or not an integer", path)
	}
	if ver != AllocationFileSchemaVersion {
		return AllocationFile{}, fmt.Errorf("%s: schema_version %d is not supported (this build reads %d)", path, ver, AllocationFileSchemaVersion)
	}
	f.SchemaVersion = ver
	f.Pool, _ = tree["pool"].(string)
	if f.Pool != pool {
		return AllocationFile{}, fmt.Errorf("%s: pool is %q, want %q", path, f.Pool, pool)
	}
	raw, isList := tree["allocations"].([]any)
	if tree["allocations"] != nil && !isList {
		return AllocationFile{}, fmt.Errorf("%s: allocations must be a list", path)
	}
	owners, values := map[string]bool{}, map[int]string{}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return AllocationFile{}, fmt.Errorf("%s: allocations[%d] must be an object", path, i)
		}
		var e AllocationEntry
		e.Owner, _ = m["owner"].(string)
		if e.Owner == "" {
			return AllocationFile{}, fmt.Errorf("%s: allocations[%d]: owner is missing", path, i)
		}
		if e.Value, ok = asInt(m["value"]); !ok {
			return AllocationFile{}, fmt.Errorf("%s: allocations[%d] (%s): value is missing or not an integer", path, i, e.Owner)
		}
		if at := scalarToString(m["allocated_at"]); at != "" {
			t, err := time.Parse(time.RFC3339, at)
			if err != nil {
				return AllocationFile{}, fmt.Errorf("%s: allocations[%d] (%s): allocated_at %q is not RFC 3339", path, i, e.Owner, at)
			}
			e.AllocatedAt = t
		}
		if owners[e.Owner] {
			return AllocationFile{}, fmt.Errorf("%s: owner %q appears twice", path, e.Owner)
		}
		if other, dup := values[e.Value]; dup {
			return AllocationFile{}, fmt.Errorf("%s: value %d is held by both %q and %q", path, e.Value, other, e.Owner)
		}
		owners[e.Owner], values[e.Value] = true, e.Owner
		f.Allocations = append(f.Allocations, e)
	}
	sortAllocations(f.Allocations)
	return f, nil
}

// asInt reads an integer scalar of the decoded tree (JSON numbers are
// float64; the YAML subset may give an int or a numeric string).
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return int(t), true
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}

func sortAllocations(es []AllocationEntry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Value != es[j].Value {
			return es[i].Value < es[j].Value
		}
		return es[i].Owner < es[j].Owner
	})
}

// EmitAllocationFile writes f as plain YAML, allocations sorted by value so
// the same allocations always give the same bytes and a diff reads as the
// values that came and went.  Strings are double-quoted (an owner holds '#',
// which YAML would otherwise read as a comment).
func EmitAllocationFile(f AllocationFile) []byte {
	es := append([]AllocationEntry(nil), f.Allocations...)
	sortAllocations(es)
	var b strings.Builder
	b.WriteString(AllocationFileHeader + "\n")
	b.WriteString("# 採番の結果の書き戻し（§19.7）。この pool の割当の正はこのファイルで、採番の store はその写しです。\n")
	b.WriteString("# 書式は docs/allocation-file.md。store を失ったら `sakutei allocator import` でここから作り直します。\n")
	fmt.Fprintf(&b, "schema_version: %d\n", AllocationFileSchemaVersion)
	fmt.Fprintf(&b, "pool: %s\n", strconv.Quote(f.Pool))
	// No allocations is "allocations:" with nothing under it (null): the
	// YAML subset the loader reads has no flow style ("[]").
	b.WriteString("allocations:\n")
	for _, e := range es {
		fmt.Fprintf(&b, "  - owner: %s\n", strconv.Quote(e.Owner))
		fmt.Fprintf(&b, "    value: %d\n", e.Value)
		if !e.AllocatedAt.IsZero() {
			fmt.Fprintf(&b, "    allocated_at: %s\n", strconv.Quote(e.AllocatedAt.UTC().Format(time.RFC3339)))
		}
	}
	return []byte(b.String())
}
