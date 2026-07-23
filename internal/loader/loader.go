// Copyright 2026 The Sakutei Authors
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
// IMPORTANT: This is a Sprint 1 provisional format, intentionally minimal.
// The production Loader (round-trip, JSON Schema validation, CUE support) is
// Story S33266e and replaces this package.  Do not add features here that
// belong to S33266e.
//
// Intent file format (JSON, provisional):
//
//	{
//	  "resources": [
//	    {
//	      "provider": "fake",
//	      "scope": {"site": "tokyo"},   // optional; omit or {} for root scope
//	      "id": "my-resource",
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
package loader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/core"
)

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
	// Fields is the map of field name → desired value (string).
	Fields map[string]string `json:"fields"`
}

// LoadDir reads all JSON files in dir and returns the desired resources.
//
// Files are processed in lexicographic order for deterministic output.
// Non-JSON files (by extension) are silently skipped.
// Subdirectories are not traversed.
func LoadDir(dir string) ([]diff.DesiredResource, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("loader: read intent dir %s: %w", dir, err)
	}

	var resources []diff.DesiredResource
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}

		path := filepath.Join(dir, name)
		loaded, err := loadFile(path)
		if err != nil {
			return nil, fmt.Errorf("loader: %w", err)
		}
		resources = append(resources, loaded...)
	}
	return resources, nil
}

// loadFile parses a single intent JSON file and returns its resources.
func loadFile(path string) ([]diff.DesiredResource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read intent file %s: %w", path, err)
	}

	var intent intentFile
	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, fmt.Errorf("parse intent file %s: %w", path, err)
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

		resources = append(resources, diff.DesiredResource{
			ID:     id,
			Fields: fields,
		})
	}
	return resources, nil
}
