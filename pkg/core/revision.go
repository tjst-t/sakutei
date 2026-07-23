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

// RevisionInfo carries the two revision identifiers that are the foundation
// of cause-tag attribution (§9.4, ADR-0008).
//
// intent_revision is the identifier of the human-authored intent.  In v1a it
// is a SHA-256 hex digest of the intent file contents (sorted deterministically
// by file name).  The S33266e Loader will replace this with a git commit hash.
//
// rendered_revision is the identifier of the fully-resolved desired state
// (intent + external inputs such as Vault secret generations, inventory hashes,
// and allocator assignments).  In v1a there are no external inputs, so
// rendered_revision equals intent_revision.  When the render pipeline
// (S072a14) is implemented, external inputs will be mixed into this hash and
// the two values will diverge for cause-tag E detection.
//
// IMPORTANT — v1a暫定:
//
//	rendered_revision の算出定義は §26 カテゴリ A（実装前に必ず決める）に分類される。
//	v1a では外部入力がないため rendered_revision = intent_revision（同値）とする暫定実装であり、
//	この等価関係は S072a14（render パイプライン）以降で変化する。scope 単位の seam
//	（ComputeRenderedRevision のシグネチャ）だけを今切る。
//
// RevisionInfo is computed once per LoadDir call (or once per render cycle)
// and is S3 state (in-memory only, never persisted — ADR-0015).
type RevisionInfo struct {
	// IntentRevision is the deterministic hash of the human-authored intent
	// (file contents, sorted by filename).
	IntentRevision string

	// RenderedRevision is the deterministic hash of intent + resolved external
	// inputs, computed per scope.  In v1a this always equals IntentRevision
	// (seam only; external inputs enter at S072a14).
	RenderedRevision string
}
