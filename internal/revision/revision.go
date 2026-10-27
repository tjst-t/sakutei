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

// Package revision computes intent_revision and rendered_revision for
// Sakutei's cause-tag attribution (§9.4, ADR-0008, Story Scb0ecd-1).
//
// # intent_revision
//
// A SHA-256 hex digest of intent file contents, sorted deterministically by
// file name.  The hash is computed over the raw bytes of each file, prefixed
// by a length-delimited header so that adjacent files cannot alias each other.
// The same intent directory always produces the same hash (deterministic);
// changing any byte in any intent file changes the hash.
//
// The S33266e Loader will replace this provisional file-content hash with the
// git commit hash that introduced the intent revision.
//
// # rendered_revision (v1a seam)
//
// In v1a there are no external inputs (Vault secret generations, inventory
// hashes, allocator assignments), so rendered_revision = intent_revision.
//
// The function signature ComputeRenderedRevision(scope, intentHash,
// externalInputs) is cut now so the seam exists.  S072a14 (render pipeline)
// fills externalInputs and the two revisions diverge, enabling cause-tag E.
//
// # IMPORTANT — rendered_revision 算出定義は §26 カテゴリ A
//
// The canonical definition of what is included in rendered_revision is a
// "must decide before implementation" item (§26 category A).  The v1a
// decision (rendered_revision = intent_revision) is explicitly provisional and
// must be revisited at S072a14.
package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// ComputeIntentRevision computes a deterministic SHA-256 hex digest of all
// intent files (.json / .yaml / .yml) in dir, sorted by filename.
//
// The digest is computed over:
//
//	for each file (sorted by name):
//	    write "<filename>:<len>\n" header
//	    write file contents verbatim
//
// This ensures:
//   - Two directories with identical file sets and identical contents always
//     produce the same digest (determinism).
//   - Adding, removing, or changing any byte in any file changes the digest.
//   - Files whose names differ but whose contents are identical do NOT alias.
//
// Files with other extensions and subdirectories are silently skipped — the
// same set of files loader.LoadDir reads.  (Until 2026-10 only .json was
// hashed, so editing a YAML intent left intent_revision unchanged and the
// change was misclassified as drift, cause B, instead of intent, cause A.)
//
// Returns an error if the directory cannot be read or any listed file cannot
// be opened.  Returns an all-zero hash if the directory contains no JSON
// files (empty intent).
func ComputeIntentRevision(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("revision: read intent dir %s: %w", dir, err)
	}

	// Collect intent file names, already in lexicographic order from ReadDir.
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !isIntentFile(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	return hashIntentFiles(dir, names)
}

// IntentTreeFiles returns the intent files under dir and all its
// subdirectories, as slash-separated paths relative to dir in sorted order.
// This is the git layout intent/<scope>/... (§17.4), where a sparse checkout
// leaves only the shard's own scope directories on disk.  A missing dir is an
// error, never an empty intent (an absent tree must not render as "desire
// nothing", §9.2).
func IntentTreeFiles(dir string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Hidden directories (.git and the like) are never intent.
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isIntentFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("revision: walk intent tree %s: %w", dir, err)
	}
	sort.Strings(rels)
	return rels, nil
}

// ComputeIntentTreeRevision is ComputeIntentRevision over the whole tree
// under dir (IntentTreeFiles).  For a tree with no subdirectories it equals
// ComputeIntentRevision(dir).
func ComputeIntentTreeRevision(dir string) (string, error) {
	rels, err := IntentTreeFiles(dir)
	if err != nil {
		return "", err
	}
	return hashIntentFiles(dir, rels)
}

// hashIntentFiles hashes the given files (slash-separated, relative to dir).
func hashIntentFiles(dir string, names []string) (string, error) {
	// Ensure deterministic order regardless of ReadDir implementation.
	names = append([]string(nil), names...)
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		path := filepath.Join(dir, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("revision: read intent file %s: %w", path, err)
		}
		// Length-delimited header so files cannot alias each other.
		header := fmt.Sprintf("%s:%d\n", name, len(data))
		if _, err := io.WriteString(h, header); err != nil {
			return "", fmt.Errorf("revision: hash header %s: %w", name, err)
		}
		if _, err := h.Write(data); err != nil {
			return "", fmt.Errorf("revision: hash body %s: %w", name, err)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// ComputeRenderedRevision returns the rendered_revision for a given scope.
//
// Parameters:
//   - scope:          the Scope unit for which rendered_revision is computed
//     (§9.4: rendered_revision is scope-unit).
//   - intentHash:     the intent_revision for this scope (from ComputeIntentRevision).
//   - externalInputs: additional deterministic hashes from external inputs
//     (Vault secret generation digests, inventory hash,
//     allocator epoch, etc.).  In v1a this is nil or empty.
//
// v1a behaviour: externalInputs is empty, so rendered_revision = intentHash.
//
// S072a14 will populate externalInputs with real external input hashes.  When
// externalInputs is non-empty, the function mixes them deterministically into
// the hash so that a secret rotation or inventory change without a matching
// intent commit produces a different rendered_revision (enabling cause-tag E).
//
// IMPORTANT — 暫定 (v1a):
//
//	rendered_revision の canonical 定義は §26 カテゴリ A。
//	v1a では外部入力がないため rendered_revision = intent_revision。
//	S072a14 で externalInputs が埋まると差が生じる。
func ComputeRenderedRevision(scope core.Scope, intentHash string, externalInputs []string) string {
	// v1a fast path: no external inputs → rendered_revision = intent_revision.
	if len(externalInputs) == 0 {
		return intentHash
	}

	// S072a14 path: mix scope, intentHash, and external inputs deterministically.
	// External inputs are sorted so that insertion order in callers cannot cause
	// hash instability (TTL cache hit/miss must not change the hash).
	sorted := make([]string, len(externalInputs))
	copy(sorted, externalInputs)
	sort.Strings(sorted)

	h := sha256.New()
	header := fmt.Sprintf("scope:%s\nintent:%s\n", scope.Path(), intentHash)
	_, _ = io.WriteString(h, header)
	for _, ext := range sorted {
		_, _ = io.WriteString(h, ext+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Compute returns a core.RevisionInfo for the given intent directory and scope.
//
// This is the primary entry point used by the loader and the reconcile loop.
// It calls ComputeIntentRevision, then ComputeRenderedRevision with empty
// external inputs (v1a: rendered_revision = intent_revision).
func Compute(dir string, scope core.Scope) (core.RevisionInfo, error) {
	intentHash, err := ComputeIntentRevision(dir)
	if err != nil {
		return core.RevisionInfo{}, err
	}
	return revisionInfo(scope, intentHash), nil
}

// ComputeTree is Compute over the whole intent tree under dir
// (ComputeIntentTreeRevision, §17.4 git layout).
func ComputeTree(dir string, scope core.Scope) (core.RevisionInfo, error) {
	intentHash, err := ComputeIntentTreeRevision(dir)
	if err != nil {
		return core.RevisionInfo{}, err
	}
	return revisionInfo(scope, intentHash), nil
}

func revisionInfo(scope core.Scope, intentHash string) core.RevisionInfo {
	rendered := ComputeRenderedRevision(scope, intentHash, nil)
	return core.RevisionInfo{
		IntentRevision:   intentHash,
		RenderedRevision: rendered,
	}
}

// isIntentFile reports whether name is an intent file the loader reads.  Keep
// in sync with loader.fileFormat.
func isIntentFile(name string) bool {
	return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
