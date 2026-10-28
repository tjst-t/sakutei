// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This file is the secret-generation step of render phase 1 (§9.1, §20.2,
// #188) and the process-wide set of configured secret backends.
//
// A secret backend configured for the run (the secret_backends: section of
// --provider-config; Vault KV v2, an outside command, or one added through
// pkg/cli.WithSecretBackend) is named by the SecretRefs that use it
// ("secretref:vault/app/db#password").  Render phase 1 reads the current
// generation of each such ref the intent writes without "#gen<N>" — metadata
// only, never the value — and writes it into the ref, so a new version of the
// secret is a diff (cause E) and gets Applied.  A ref whose generation cannot
// be read (backend unreachable, sealed, the secret missing) leaves its
// resource unknown this cycle: neither Observed nor Applied, its downstream
// skipped, never pruned (§20.3, unknown ≠ absent).
//
// The value is resolved only at Apply, by the provider, through the same
// configured backend (FileSecretBackend.ResolveValue delegates refs of a
// configured backend name to it).

package render

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// GenerationalSecretBackend is a configured secret backend (#188).  It is the
// shape of pkg/sdk.SecretBackend.
type GenerationalSecretBackend interface {
	// Generation returns the current generation of the secret at path
	// (metadata only: the value is never read).
	Generation(ctx context.Context, path string) (int64, error)
	// Resolve returns the value of the secret at path, at the given
	// generation when it is not 0.  Called only at Apply time (§20.2).
	Resolve(ctx context.Context, path string, generation int64) (string, error)
}

var (
	secretBackendsMu sync.RWMutex
	secretBackends   map[string]GenerationalSecretBackend
)

// UseSecretBackends sets the secret backends configured for this process,
// by the backend name SecretRefs use.  The CLI calls it once at start-up.
func UseSecretBackends(m map[string]GenerationalSecretBackend) {
	cp := make(map[string]GenerationalSecretBackend, len(m))
	for k, v := range m {
		cp[k] = v
	}
	secretBackendsMu.Lock()
	secretBackends = cp
	secretBackendsMu.Unlock()
}

// SecretBackends returns the secret backends configured for this process.
func SecretBackends() map[string]GenerationalSecretBackend {
	secretBackendsMu.RLock()
	defer secretBackendsMu.RUnlock()
	return secretBackends
}

// configuredSecretBackend returns the configured backend of that name.
func configuredSecretBackend(name string) (GenerationalSecretBackend, bool) {
	secretBackendsMu.RLock()
	defer secretBackendsMu.RUnlock()
	b, ok := secretBackends[name]
	return b, ok
}

// UnresolvedSecret is a SecretRef whose generation could not be read.
type UnresolvedSecret struct {
	ID    core.ID
	Field string
	// Ref is the masked ref (never a value).
	Ref string
	Err error
}

// SecretGenResult is the outcome of ResolveSecretGenerations.
type SecretGenResult struct {
	// Resources are copies of the input with each generation written into
	// its ref.
	Resources []diff.DesiredResource
	// Inputs are "secret:<backend>/<path>=<gen>" strings, sorted, for
	// revision.ComputeRenderedRevision: a new version is an external input
	// change (cause E, §9.4).
	Inputs []string
	// Unresolved lists the refs whose generation could not be read, sorted.
	// Their resources are unknown this cycle.
	Unresolved []UnresolvedSecret
}

// UnknownLocals returns the Local IDs that must not be converged this cycle
// because a secret generation could not be read: every such resource and,
// for a composite, the children it expands to.
func (r SecretGenResult) UnknownLocals(parentToChildren map[string][]string) map[string]bool {
	out := make(map[string]bool, len(r.Unresolved))
	for _, u := range r.Unresolved {
		out[u.ID.Local] = true
		for _, child := range parentToChildren[u.ID.Local] {
			out[child] = true
		}
	}
	return out
}

// Reason is the one-line reason of an unresolved resource's skip.
func (u UnresolvedSecret) Reason() string {
	return "secret generation of " + u.Ref + " in field " + strconv.Quote(u.Field) +
		" could not be read: " + u.Err.Error() + " (unknown: not Observed or Applied until it can, §20.3)"
}

// SecretUnknownSkipReason is the skip reason of a resource held because a
// secret generation could not be read.
const SecretUnknownSkipReason = "unknown: a secret generation could not be read (not Observed or Applied until it can, §20.3)"

// ResolveSecretGenerations is the secret-generation step of render phase 1
// (§9.1, §20.2).  For every field holding a SecretRef of a backend in
// backends without a generation, it reads the current generation and writes
// it into the ref.  A ref that pins a generation ("#gen<N>") is left as it is.
// Each secret's generation is read once per call.
func ResolveSecretGenerations(ctx context.Context, backends map[string]GenerationalSecretBackend, resources []diff.DesiredResource) SecretGenResult {
	var res SecretGenResult
	if len(backends) == 0 {
		res.Resources = resources
		return res
	}
	type got struct {
		gen int64
		err error
	}
	seen := map[string]got{}
	inputs := map[string]bool{}
	res.Resources = make([]diff.DesiredResource, 0, len(resources))
	for _, r := range resources {
		var fields map[string]string
		for _, field := range sortedKeys(r.Fields) {
			v := r.Fields[field]
			if !looksLikeSecretRef(v) {
				continue
			}
			ref, err := core.ParseSecretRef(v)
			if err != nil || ref.Generation() != 0 {
				continue
			}
			b, ok := backends[ref.Backend()]
			if !ok {
				continue
			}
			key := ref.Backend() + "/" + ref.Path()
			g, done := seen[key]
			if !done {
				g.gen, g.err = b.Generation(ctx, ref.Path())
				if g.err == nil && g.gen <= 0 {
					g.err = errText("the backend returned no generation")
				}
				seen[key] = g
			}
			if g.err != nil {
				res.Unresolved = append(res.Unresolved, UnresolvedSecret{ID: r.ID, Field: field, Ref: "secretref:" + key, Err: g.err})
				continue
			}
			if fields == nil {
				fields = make(map[string]string, len(r.Fields))
				for k, x := range r.Fields {
					fields[k] = x
				}
			}
			fields[field] = ref.WithGeneration(g.gen).String()
			inputs["secret:"+key+"="+strconv.FormatInt(g.gen, 10)] = true
		}
		if fields != nil {
			r.Fields = fields
		}
		res.Resources = append(res.Resources, r)
	}
	for in := range inputs {
		res.Inputs = append(res.Inputs, in)
	}
	sort.Strings(res.Inputs)
	sort.Slice(res.Unresolved, func(i, j int) bool {
		a, b := res.Unresolved[i], res.Unresolved[j]
		if a.ID.String() != b.ID.String() {
			return a.ID.String() < b.ID.String()
		}
		return a.Field < b.Field
	})
	return res
}

// looksLikeSecretRef reports whether v is written as a SecretRef.
func looksLikeSecretRef(v string) bool {
	t := strings.TrimSpace(v)
	return strings.HasPrefix(t, "secretref:") || (strings.HasPrefix(t, "{") && strings.Contains(t, `"$secret"`))
}
