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

package main

// causeState holds the "previous cycle" values needed to classify cause tags
// A/B/C for each resource diff (§10.2, ADR-0008, Scb0ecd-2).
//
// It is persisted to a small JSON file (--cause-state <path>) so that
// subsequent invocations (plan → reconcile → next reconcile) can compare
// "current" against "previous".
//
// S3 semantics (ADR-0015): this file is optional and its loss is safe.
// If the file is absent or unreadable, all Prev* fields default to empty
// string, which means:
//   - IntentPrevRevision = "" → intent always "changed" → all tags A (correct
//     for first-run: the operator is declaring desired state for the first time).
//   - ProviderPrevVersion = "" → no version change detectable → C not fired
//     on the first run (correct: we cannot know what "previous" version was).
//
// This file is written at the END of each plan/reconcile invocation so that
// the next invocation can compare against it.  It is never written by the
// daemon (the daemon maintains its own in-memory state; this file is for the
// one-shot CLI paths).
import (
	"encoding/json"
	"errors"
	"os"
)

// causeStateFile is the on-disk JSON format.
type causeStateFile struct {
	// PrevIntentRevision is the intent_revision from the previous invocation.
	PrevIntentRevision string `json:"prev_intent_revision,omitempty"`
	// PrevRenderedRevision is the rendered_revision from the previous invocation.
	PrevRenderedRevision string `json:"prev_rendered_revision,omitempty"`
	// PrevProviderVersion is the provider_version from the previous invocation.
	PrevProviderVersion string `json:"prev_provider_version,omitempty"`
}

// loadCauseState reads the cause-state file at path.
// Returns a zero-value causeStateFile if the file is absent or empty (safe
// first-run behaviour).  Returns an error only for genuine I/O or parse
// failures on an existing non-empty file.
func loadCauseState(path string) (causeStateFile, error) {
	if path == "" {
		return causeStateFile{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return causeStateFile{}, nil
	}
	if err != nil {
		return causeStateFile{}, err
	}
	if len(data) == 0 {
		return causeStateFile{}, nil
	}
	var s causeStateFile
	if err := json.Unmarshal(data, &s); err != nil {
		return causeStateFile{}, err
	}
	return s, nil
}

// saveCauseState writes cur to path as JSON.
// A write failure is non-fatal: the next invocation will see empty Prev*
// fields, which causes all tags to fall back to A (safe: first-run semantics).
func saveCauseState(path string, cur causeStateFile) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
