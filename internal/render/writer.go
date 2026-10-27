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

package render

// This file implements the generated/ output convention (§9.6).
//
// Rules (§9.6, decisions.json S072a14 "generated/ 規約の実装方針"):
//   - Output files must include the auto-generation header fields.
//   - The loader must reject (warn) files missing the header.
//   - The output is deterministic: same input → bit-for-bit identical file.
//   - generated/ is separate from intent/ (never mix human-authored files with
//     generated ones in the same directory).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// GeneratedFileName is the canonical name of the render phase 2 output file
// within the generated/ directory.
const GeneratedFileName = "expanded.json"

// AutoGenHeader contains the required header fields for generated/ files (§9.6,
// ADR-0008).  Any file in generated/ that lacks _generated:true is considered
// to have been edited by a human and must be rejected or overwritten.
const (
	HeaderGenerated        = "_generated"
	HeaderGeneratedBy      = "_generated_by"
	HeaderDoNotEdit        = "_do_not_edit"
	HeaderGeneratedByValue = "sakutei render"
)

// generatedRecord is the on-disk JSON structure for generated/expanded.json.
// It carries the mandatory auto-generation header fields (§9.6) plus the list
// of expanded child resource descriptors.
type generatedRecord struct {
	// Auto-generation header (§9.6) — required on read; always written.
	Generated   bool   `json:"_generated"`
	GeneratedBy string `json:"_generated_by"`
	DoNotEdit   bool   `json:"_do_not_edit"`

	// Resources holds the expanded child resource declarations (non-composite
	// resources only; composite parents are excluded from the file).
	Resources []generatedResource `json:"resources"`
}

// generatedResource captures the fields written per child in generated/expanded.json.
type generatedResource struct {
	Provider  string            `json:"provider"`
	ID        string            `json:"id"`
	Scope     map[string]string `json:"scope,omitempty"`
	Fields    map[string]string `json:"fields"`
	ManagedBy string            `json:"managed_by"`
}

// WriteGenerated writes the expanded child resources to <generatedDir>/expanded.json.
//
// Only non-composite resources (IsComposite==false) are included.  The output
// includes the mandatory auto-generation header (§9.6).  The JSON output is
// stable: keys are marshalled in a deterministic order (Go's json package sorts
// struct fields by definition; map keys within "fields" and "scope" are also
// sorted alphabetically to ensure bit-for-bit reproducibility).
//
// If generatedDir does not exist it is created (MkdirAll).  An error is
// returned if the directory cannot be created or the file cannot be written.
func WriteGenerated(generatedDir string, expanded []ExpandedResource) error {
	// Collect only non-composite, managed children (those with ManagedBy set).
	// Resources that are plain (not expanded children) are NOT written to
	// generated/ — they live in intent/ and are not re-exported here.
	//
	// F-1: initialise as an empty (non-nil) slice so that when there are no
	// synthetic children the JSON output is "resources": [] rather than
	// "resources": null (a nil slice marshals to null).  An empty array keeps
	// the generated file well-formed and diff-stable across cycles.
	children := make([]generatedResource, 0)
	for _, r := range expanded {
		if r.IsComposite {
			continue // composite parents are not written to generated/
		}
		if r.ManagedBy == "" {
			continue // plain intent resource — stays in intent/
		}
		// Sort field keys for deterministic output.
		sortedFields := sortedMapCopy(r.Fields)
		sortedScope := sortedMapCopy(r.ID.Scope.Labels)

		children = append(children, generatedResource{
			Provider:  r.ID.Provider,
			ID:        r.ID.Local,
			Scope:     sortedScope,
			Fields:    sortedFields,
			ManagedBy: r.ManagedBy,
		})
	}

	// Sort children by (provider, id) for deterministic ordering.
	sort.Slice(children, func(i, j int) bool {
		if children[i].Provider != children[j].Provider {
			return children[i].Provider < children[j].Provider
		}
		return children[i].ID < children[j].ID
	})

	rec := generatedRecord{
		Generated:   true,
		GeneratedBy: HeaderGeneratedByValue,
		DoNotEdit:   true,
		Resources:   children,
	}

	// Marshal with stable, pretty-printed output.
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("render: marshal generated output: %w", err)
	}

	// Ensure the generated directory exists.
	if err := os.MkdirAll(generatedDir, 0755); err != nil {
		return fmt.Errorf("render: create generated dir %s: %w", generatedDir, err)
	}

	outPath := filepath.Join(generatedDir, GeneratedFileName)
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return fmt.Errorf("render: write %s: %w", outPath, err)
	}

	return nil
}

// ValidateGeneratedFile reads the given file and returns an error if it does
// not contain the mandatory auto-generation header (§9.6).  Returns nil if the
// file has a valid header or does not exist.
//
// Callers (the loader) use this to detect direct human edits to generated/
// files (two-management prevention).  An invalid header triggers a warning
// event; the file is overwritten on the next render cycle regardless.
func ValidateGeneratedFile(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // absent is fine
	}
	if err != nil {
		return fmt.Errorf("render: read %s for validation: %w", path, err)
	}

	var hdr struct {
		Generated bool `json:"_generated"`
	}
	if err := json.Unmarshal(data, &hdr); err != nil {
		return fmt.Errorf("render: generated file %s is not valid JSON (human edit?): %w", path, err)
	}
	if !hdr.Generated {
		return fmt.Errorf("render: generated file %s is missing _generated:true header — direct human edit detected; file will be overwritten on next render", path)
	}
	return nil
}

// sortedMapCopy returns a copy of m with keys in a stable order.
// This is used for deterministic JSON output: Go's encoding/json already emits
// the keys of a map[string]string in alphabetical order, so the copy is a
// belt-and-suspenders measure that makes the deterministic-output contract
// explicit at the call site rather than relying on that behaviour implicitly.
func sortedMapCopy(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	// Go's encoding/json emits map[string]string keys in alphabetical order,
	// but we return a copy anyway to make the deterministic-output contract explicit.
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
