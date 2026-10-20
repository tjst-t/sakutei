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
// Package syncmode implements Sync Mode (§24.5): for every scope declared
// `mode: sync`, record reality into generated/ — one way, reality → files,
// never connected to render / Diff / Apply.
//
// It reuses import's observation (internal/importer): List the scope, Read
// each resource, write a snapshot.  A snapshot is rewritten only when it
// changed (step 2-3 of §24.5).  Committing the snapshot to git is not done
// here yet (Backlog #35).
//
// generated/ discipline (§9.6, §24.5): the snapshot carries an auto-generated
// header with the SHA-256 of its body.  If the body no longer matches — a
// person edited the file — Sync refuses to overwrite it instead of silently
// discarding the edit.
package syncmode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/internal/importer"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// HeaderMarker is the first line of every Sync Mode snapshot.
const HeaderMarker = "# sakutei:generated sync"

const bodyHashPrefix = "# body-sha256: "

// ErrEditedByHand is returned (wrapped) when a snapshot's body does not match
// the hash in its header, or the file lacks the header.
var ErrEditedByHand = errors.New("syncmode: snapshot was edited by hand (generated/ is auto-generated only)")

// Options configures one Sync pass.
type Options struct {
	// Intent is the loaded intent; its mode: sync scopes are synced.
	Intent loader.Intent
	// Root is the repository root; target_path is resolved against it
	// (the parent of the intent directory).
	Root string
	// Providers to record, keyed by provider name.  Each must support List.
	Providers map[string]provider.Provider
}

// Outcome is the result for one (scope, provider) pair.
type Outcome struct {
	Scope     string
	Provider  string
	Path      string
	Resources int
	Changed   bool
	// Err is set when this pair was skipped (incomplete List, hand-edited
	// snapshot, write failure).  The previous snapshot is left untouched.
	Err error
}

// RunOnce performs one Sync pass over every sync scope and provider.  A
// failure for one pair does not stop the others; it is reported in its
// Outcome and the previous snapshot stays as it was (never write a shrunken
// or partial view of reality).
func RunOnce(ctx context.Context, opts Options) []Outcome {
	names := make([]string, 0, len(opts.Providers))
	for n := range opts.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []Outcome
	for _, sp := range opts.Intent.SyncScopes() {
		dir := filepath.Join(opts.Root, filepath.FromSlash(sp.TargetPath))
		for _, name := range names {
			o := Outcome{Scope: sp.Scope.String(), Provider: name, Path: filepath.Join(dir, "sync-"+name+".yaml")}
			res, err := importer.Run(ctx, importer.Options{
				Providers: map[string]provider.Provider{name: opts.Providers[name]},
				Selector:  sp.Scope,
				Samples:   1,
			})
			if err != nil {
				o.Err = err
				out = append(out, o)
				continue
			}
			o.Resources = len(res.Resources)
			body := importer.EmitResourcesYAML(res.Resources)
			o.Changed, o.Err = writeSnapshot(o.Path, sp.Scope.String(), name, body)
			out = append(out, o)
		}
	}
	return out
}

// writeSnapshot writes the snapshot if its body changed.  It refuses to
// overwrite a file whose body does not match its header hash.
func writeSnapshot(path, scope, providerName, body string) (bool, error) {
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])

	if old, err := os.ReadFile(path); err == nil {
		oldHash, oldBody, ok := splitSnapshot(old)
		if !ok {
			return false, fmt.Errorf("%w: %s has no Sync Mode header; move it out of the sync target", ErrEditedByHand, path)
		}
		oldSum := sha256.Sum256(oldBody)
		if hex.EncodeToString(oldSum[:]) != oldHash {
			return false, fmt.Errorf("%w: %s; restore it or delete it so Sync can rewrite it", ErrEditedByHand, path)
		}
		if oldHash == hash {
			return false, nil
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	var b strings.Builder
	b.WriteString(HeaderMarker + "\n")
	b.WriteString("# Sync Mode（§24.5）が現実から自動で書き出したスナップショットです。手で編集しないでください\n")
	b.WriteString("# （次の反映で上書きされるため、編集を検知したら上書きを止めます）。\n")
	b.WriteString("# converge に切り替えるときは、人がレビューして intent/ へ移します（§24.5 cutover）。\n")
	fmt.Fprintf(&b, "# scope: %s\n", scope)
	fmt.Fprintf(&b, "# provider: %s\n", providerName)
	b.WriteString(bodyHashPrefix + hash + "\n")
	b.WriteString(body)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := writeAtomic(path, []byte(b.String())); err != nil {
		return false, err
	}
	return true, nil
}

// splitSnapshot returns the header hash and the body of a snapshot file.
// ok is false when the file does not start with the Sync Mode header.
func splitSnapshot(data []byte) (hash string, body []byte, ok bool) {
	if !bytes.HasPrefix(data, []byte(HeaderMarker+"\n")) {
		return "", nil, false
	}
	rest := data
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return "", nil, false
		}
		line := string(rest[:nl])
		rest = rest[nl+1:]
		if strings.HasPrefix(line, bodyHashPrefix) {
			return strings.TrimPrefix(line, bodyHashPrefix), rest, true
		}
		if !strings.HasPrefix(line, "#") {
			return "", nil, false
		}
	}
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o644)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
