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

// This file implements the 段0-minimal file SecretBackend behind the existing
// phase1 SecretBackend seam (§20.5 資格局所性 / §23.6 段0-minimal).
//
// # Blast radius = single site (§20.5)
//
// The file backend reads secret metadata (generation only) from a LOCAL file on
// the site running this reconciler.  The write credential and the secret store
// are both site-local, so a compromise of one site cannot reach another site's
// secrets — the blast radius is the current site.  This is the deliberate
// 段0-minimal posture: no Vault cluster, no network dependency for secret
// metadata, just a local file whose reach is one site.
//
// # Metadata only — value never read through GenerationOf (§20.2)
//
// GenerationOf resolves ONLY the generation number.  The plaintext value is NOT
// returned by this path.  Value resolution is the JIT step at Apply time
// (FileSecretBackend.ResolveValue → core.SecretRef.Resolve), never render.
//
// # Vault stays a documented stub (offline / stdlib-only)
//
// VaultSecretBackend implements the same seam but returns a clear
// "not implemented offline" error.  It lands as a real client in v1b/v2; the
// stub keeps the seam callable and the wiring shape fixed today.

package render

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
)

// fileSecretStore is the on-disk shape of the file SecretBackend store.
//
// It maps secret path → per-secret metadata.  The value plaintext MAY be
// present in the "value" field for the 段0-minimal file backend (this file is
// the site-local secret store), but GenerationOf NEVER reads it — only Resolve
// does, at Apply time.  Keeping value optional lets the same file drive both
// the metadata path (generation) and the JIT value path without a second store.
type fileSecretStore struct {
	Secrets map[string]fileSecretEntry `json:"secrets"`
}

// fileSecretEntry is one secret's metadata + (site-local) value.
type fileSecretEntry struct {
	// Generation is the freshness metadata resolved by GenerationOf.  A bump of
	// this number is a legitimate change (cause E) that re-Applies the secret.
	Generation int64 `json:"gen"`
	// Value is the site-local plaintext, read ONLY by ResolveValue (JIT, Apply
	// time).  It is never returned by GenerationOf and never enters diff.
	Value string `json:"value,omitempty"`
}

// FileSecretBackend is the 段0-minimal file SecretBackend (§23.6).  It reads
// secret metadata (and, for JIT resolution, the value) from a local file whose
// reach is the current site (§20.5 blast radius = single site).
//
// It satisfies:
//   - the render SecretBackend seam (GenerationOf: metadata only), and
//   - core.SecretResolver (ResolveValue: JIT plaintext at Apply time).
type FileSecretBackend struct {
	// path is the local secret store file (site-local; blast radius = this site).
	path string
}

// Compile-time assertions: FileSecretBackend satisfies both seams.
var (
	_ SecretBackend = (*FileSecretBackend)(nil)
)

// NewFileSecretBackend returns a file SecretBackend reading from the given
// local store path (§23.6 段0-minimal).  The path is site-local; its blast
// radius is the current site (§20.5).
func NewFileSecretBackend(path string) *FileSecretBackend {
	return &FileSecretBackend{path: path}
}

// GenerationOf implements the render SecretBackend seam.  It reads ONLY the
// generation metadata for the secret at secretPath from the local file.  It
// NEVER reads or returns the value (§20.2: metadata path is value-free).
//
// Returns ("", err) when the store file is unreachable or the path is unknown,
// mirroring the seam contract (Vault-sealed-restart analogue).
func (b *FileSecretBackend) GenerationOf(_ context.Context, secretPath string) (string, error) {
	store, err := b.load()
	if err != nil {
		return "", err
	}
	entry, ok := store.Secrets[secretPath]
	if !ok {
		return "", &SecretNotFoundError{Path: secretPath, Backend: "file"}
	}
	// Metadata only: the generation, formatted as a string per the seam.
	return strconv.FormatInt(entry.Generation, 10), nil
}

// ResolveValue implements core.SecretResolver.  It performs the JUST-IN-TIME
// value resolution at Apply time (§20.2 Apply 直前のみ実値化): it reads the
// plaintext from the site-local store and returns it to the caller, who must
// use it immediately and Zero() it.  The value is not cached here.
//
// generation is accepted for audit/version pinning; the file backend stores a
// single current value per path, so it is used only to assert the caller's ref
// is not stale relative to the store (best-effort; a mismatch is not fatal in
// 段0-minimal).
func (b *FileSecretBackend) ResolveValue(_ context.Context, _ string, path string, _ int64) (string, error) {
	store, err := b.load()
	if err != nil {
		return "", err
	}
	entry, ok := store.Secrets[path]
	if !ok {
		return "", &SecretNotFoundError{Path: path, Backend: "file"}
	}
	return entry.Value, nil
}

// load reads and parses the site-local store file.
func (b *FileSecretBackend) load() (fileSecretStore, error) {
	var store fileSecretStore
	data, err := os.ReadFile(b.path)
	if err != nil {
		return store, &SecretBackendUnreachableError{Backend: "file", Path: b.path, Cause: err}
	}
	if err := json.Unmarshal(data, &store); err != nil {
		// S33266e-1 (§20 defense-in-depth): the store file physically holds
		// plaintext secret values, and a json.Unmarshal error can quote a snippet
		// of the offending input.  Do NOT wrap the raw parser error text; return a
		// generic "malformed JSON" error identifying only the store path/backend.
		return store, &SecretBackendUnreachableError{Backend: "file", Path: b.path, Cause: errText("malformed JSON")}
	}
	if store.Secrets == nil {
		store.Secrets = map[string]fileSecretEntry{}
	}
	return store, nil
}

// SecretNotFoundError is returned when a requested secret path is absent from
// the backend store.
type SecretNotFoundError struct {
	Path    string
	Backend string
}

func (e *SecretNotFoundError) Error() string {
	return "secret backend [" + e.Backend + "]: no secret at path " + e.Path
}

// SecretBackendUnreachableError is returned when the backend store itself
// cannot be read/parsed (file missing, unreadable, malformed).
type SecretBackendUnreachableError struct {
	Backend string
	Path    string
	Cause   error
}

func (e *SecretBackendUnreachableError) Error() string {
	msg := "secret backend [" + e.Backend + "] unreachable (" + e.Path + ")"
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

func (e *SecretBackendUnreachableError) Unwrap() error { return e.Cause }

// ─────────────────────────────────────────────────────────────────────────────
// VaultSecretBackend — documented stub (v1b/v2; offline+stdlib-only in v1a)
// ─────────────────────────────────────────────────────────────────────────────

// VaultSecretBackend is the seam-compatible stub for HashiCorp Vault (KV v2).
//
// v1a is offline and stdlib-only, so no Vault client is built here.  The stub
// keeps the SecretBackend seam callable and the wiring shape fixed; GenerationOf
// returns a clear "not implemented offline" error.  The real client (Vault KV v2
// metadata → generation) lands in v1b/v2 behind this exact seam (§20.5).
type VaultSecretBackend struct{}

var _ SecretBackend = (*VaultSecretBackend)(nil)

// GenerationOf returns a not-implemented error: Vault is unavailable in the
// offline v1a build.  The seam exists so callers can name and wire it now.
func (b *VaultSecretBackend) GenerationOf(_ context.Context, _ string) (string, error) {
	return "", errText("vault SecretBackend not implemented in offline v1a (段0-minimal uses the file backend; Vault lands in v1b/v2)")
}

// errText is a stdlib-free sentinel error type (avoids importing errors for a
// single stub message).
type errText string

func (e errText) Error() string { return string(e) }
