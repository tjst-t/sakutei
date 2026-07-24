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

package core

import (
	"context"
	"encoding/json"
	"fmt"
)

// SecretRef is a *reference* to a secret value (§20, ADR-0016).  It is the
// canonical way a secret enters Sakutei's desired state.
//
// # 値は収束しない・参照/存在/鮮度のみが収束する（§20.2）
//
// A SecretRef carries ONLY the reference (backend + path) and a generation
// number (the freshness metadata resolved from the SecretBackend, §9.1
// フェーズ1).  It NEVER holds the plaintext value on any field.  This is the
// structural guarantee behind "値は収束しない":
//
//   - Only the ref (path + generation) enters Diff / convergence.  A generation
//     bump (gen3 → gen4) is a legitimate change that shows in diff and gets
//     Applied (cause tag E, §10.3).  A change of the underlying *value* WITHOUT
//     a generation bump is invisible to convergence by construction — there is
//     no value field to diff.
//   - The value is resolved JUST-IN-TIME at Apply via Resolve(ctx, backend) and
//     is returned to the caller WITHOUT being stored on the ref.  It is never
//     cached, never logged, never diffed.
//
// # 自動マスク（§20 — the ref is not secret; only the value is）
//
// String(), the fmt.Stringer / fmt.Formatter path (%v / %s / %+v), MarshalText,
// and MarshalJSON all render the REF, never the value:
//
//   - String()      → "secretref:<backend>/<path>#gen<N>"   (ref + generation visible)
//   - MarshalJSON   → {"$secret":"<path>","backend":"<backend>","gen":<N>}
//
// The raw value can never appear in any of these paths because a SecretRef has
// no exported value field and no code path that writes the value onto the ref.
type SecretRef struct {
	// backend names the SecretBackend that owns this secret (e.g. "file",
	// "vault").  It is part of the ref and is NOT secret (§20: the ref is not
	// secret; only the value is).
	backend string
	// path is the backend-relative path of the secret (e.g. "db/mysql/root").
	// It is part of the ref and is NOT secret.
	path string
	// generation is the freshness metadata resolved from the SecretBackend
	// (§9.1 フェーズ1 シークレット世代解決).  Only this participates in
	// convergence, alongside backend/path.  Zero means "generation not yet
	// resolved" (the ref is still valid; the backend supplies the generation
	// during render phase 1).
	generation int64
}

// NewSecretRef constructs a SecretRef for the given backend, path, and
// generation.  There is deliberately no constructor that accepts a plaintext
// value: a SecretRef references a secret; it never carries one.
func NewSecretRef(backend, path string, generation int64) SecretRef {
	return SecretRef{backend: backend, path: path, generation: generation}
}

// Backend returns the name of the SecretBackend that owns this secret.
// The backend name is part of the ref and is not itself secret.
func (r SecretRef) Backend() string { return r.backend }

// Path returns the backend-relative path of the secret.
// The path is part of the ref and is not itself secret.
func (r SecretRef) Path() string { return r.path }

// Generation returns the resolved generation (freshness) of the secret.
// Only the generation (with backend/path) participates in convergence.
func (r SecretRef) Generation() int64 { return r.generation }

// WithGeneration returns a copy of the ref with the generation replaced.
// Used by render phase 1 to inject the generation resolved from the
// SecretBackend without mutating the caller's ref.
func (r SecretRef) WithGeneration(generation int64) SecretRef {
	r.generation = generation
	return r
}

// String returns the auto-masked reference form: "secretref:<backend>/<path>#gen<N>".
//
// The value is NEVER included.  Because SecretRef implements fmt.Stringer, this
// form is what appears for %v and %s as well (see Format for %#v).  This is the
// single point that guarantees a SecretRef renders masked in every fmt-based
// output path (logs, plan/status render, diff render).
func (r SecretRef) String() string {
	return "secretref:" + r.backend + "/" + r.path + "#gen" + itoa(r.generation)
}

// Format implements fmt.Formatter so that even the %#v (GoString-style) verb —
// which would otherwise print the struct's unexported fields — renders the
// masked reference form.  Every fmt verb routes through String(); the value is
// never reachable via fmt.
func (r SecretRef) Format(f fmt.State, verb rune) {
	// All verbs render the masked ref form.  We intentionally do NOT expose the
	// struct internals for %#v: an operator dumping a State with %#v must not be
	// able to widen the exposure beyond the ref.
	_, _ = f.Write([]byte(r.String()))
	_ = verb
}

// MarshalText implements encoding.TextMarshaler so any text-encoding path
// (including some loggers and map[string]string coercion helpers) emits the
// masked ref form, never the value.
func (r SecretRef) MarshalText() ([]byte, error) {
	return []byte(r.String()), nil
}

// secretJSON is the canonical JSON serialization of a SecretRef (§20 confirmed
// contract): {"$secret":"<path>","backend":"<backend>","gen":<N>}.  The raw
// value is structurally absent — there is no field for it.
type secretJSON struct {
	Secret  string `json:"$secret"`
	Backend string `json:"backend,omitempty"`
	Gen     int64  `json:"gen"`
}

// MarshalJSON implements json.Marshaler.  It emits the ref (path + backend +
// generation) and NEVER the value.  This is the serialization used wherever a
// secret field is written to JSON (generated/ writer, status/plan JSON dumps).
func (r SecretRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(secretJSON{
		Secret:  r.path,
		Backend: r.backend,
		Gen:     r.generation,
	})
}

// UnmarshalJSON implements json.Unmarshaler for the {"$secret":...} form so a
// SecretRef can be read back from intent/generated JSON without ever seeing a
// value (there is no value in the serialized form).
func (r *SecretRef) UnmarshalJSON(data []byte) error {
	var s secretJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	r.path = s.Secret
	r.backend = s.Backend
	r.generation = s.Gen
	return nil
}

// SecretValue is a transient holder for a resolved plaintext secret value.
// It exists so callers cannot accidentally assign a resolved value back onto a
// SecretRef (there is no setter), and so the value's lifetime is explicit: the
// caller resolves JIT, uses it immediately, then calls Zero() (§20.2 Apply
// 直前のみ実値化).  SecretValue is never stored on a SecretRef, never logged,
// never diffed.
//
// SecretValue deliberately does NOT implement fmt.Stringer / MarshalJSON /
// MarshalText that expose the value — it masks like SecretRef.  The value is
// only reachable via the explicit Expose() accessor.
type SecretValue struct {
	// value is the resolved plaintext.  Unexported so it cannot leak through
	// struct reflection dumps except via the deliberate Expose() call.
	value []byte
}

// Expose returns the raw resolved value.  This is the ONLY way to read the
// plaintext, and it exists solely for the Apply-time hand-off to the provider.
// Callers MUST NOT log, cache, or diff the result, and MUST call Zero() when
// done (§20.2).
func (v *SecretValue) Expose() string { return string(v.value) }

// Zero overwrites the resolved value in memory so the plaintext does not linger
// after the Apply hand-off (§20.2 使用後 Zero()).  After Zero(), Expose()
// returns the empty string.
func (v *SecretValue) Zero() {
	for i := range v.value {
		v.value[i] = 0
	}
	v.value = v.value[:0]
}

// String masks SecretValue as *** so that even a resolved value cannot leak
// through fmt (%v / %s) or logging.  The plaintext is reachable ONLY via
// Expose().
func (v SecretValue) String() string { return maskedScalar }

// Format ensures every fmt verb (including %#v) masks the resolved value.
func (v SecretValue) Format(f fmt.State, verb rune) {
	_, _ = f.Write([]byte(maskedScalar))
	_ = verb
}

// MarshalJSON masks a resolved value as "***" (it must never be serialized).
func (v SecretValue) MarshalJSON() ([]byte, error) { return json.Marshal(maskedScalar) }

// maskedScalar is the human-diff / masked-value token (§20 confirmed contract:
// the masked scalar in human diff output is "***").
const maskedScalar = "***"

// SecretResolver is the JIT resolution seam.  It is satisfied by the render
// SecretBackend implementations (file backend in 段0-minimal; Vault later) via
// a thin adapter, and by fakes in tests.  Keeping the resolution behind an
// interface here avoids a core→render import cycle while letting SecretRef own
// the JIT contract.
//
// ResolveValue MUST NOT be called during Diff/convergence — only at Apply time
// (§20.2).  The returned SecretValue is owned by the caller, who must Zero() it.
type SecretResolver interface {
	// ResolveValue fetches the plaintext for the given backend/path.  The
	// generation is passed for audit/version pinning; implementations MAY use
	// it to fetch a specific version.
	ResolveValue(ctx context.Context, backend, path string, generation int64) (string, error)
}

// Resolve fetches the plaintext value JUST-IN-TIME via the resolver and returns
// it wrapped in a SecretValue.  The value is returned to the caller and is
// NEVER stored on the SecretRef (r is unchanged; its String()/JSON stay masked
// before and after this call, satisfying AC-S33266e-1-3).
//
// The caller MUST use the value immediately (Apply hand-off) and then call
// SecretValue.Zero().  Resolve does not cache: every call re-resolves.
func (r SecretRef) Resolve(ctx context.Context, resolver SecretResolver) (SecretValue, error) {
	if resolver == nil {
		return SecretValue{}, errNoResolver
	}
	v, err := resolver.ResolveValue(ctx, r.backend, r.path, r.generation)
	if err != nil {
		return SecretValue{}, err
	}
	return SecretValue{value: []byte(v)}, nil
}

// errNoResolver is returned by Resolve when no resolver is configured.
var errNoResolver = errText("secretref: no resolver configured (value cannot be resolved offline)")

// errText is a tiny stdlib-free error type (avoids importing errors just for a
// sentinel; keeps this file's import set minimal).
type errText string

func (e errText) Error() string { return string(e) }

// itoa formats a base-10 int64 without importing strconv into the hot masking
// path.  Kept local so String() has no heavyweight dependency.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
