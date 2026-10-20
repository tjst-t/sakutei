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
// each resource (several samples, as import's managed candidates do), write a
// snapshot and commit it to git as the system (git.go).
//
// A snapshot is rewritten — and committed — only when its meaningful content
// changed.  A field that changed between samples is a managed candidate
// (§24.4); it is remembered in the snapshot from then on and its value no
// longer counts as a change, so a field that only flaps never makes a commit.
//
// generated/ discipline (§9.6, §24.5): the snapshot carries an auto-generated
// header with the SHA-256 of its body.  If the body no longer matches — a
// person edited the file — Sync refuses to overwrite it instead of silently
// discarding the edit.  The same holds on the git side: if a commit that is
// not Sync's changed the snapshot, Sync stops for that snapshot.
package syncmode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/importer"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// HeaderMarker is the first line of every Sync Mode snapshot.
const HeaderMarker = "# sakutei:generated sync"

const (
	bodyHashPrefix    = "# body-sha256: "
	significantPrefix = "# significant-sha256: "
	flappingPrefix    = "# flapping-fields: "
)

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

	// Samples is how many times reality is observed per pass (>= 1); a field
	// that changes between samples becomes a managed candidate (§24.4).
	Samples int
	// Interval is the wait between samples.
	Interval time.Duration
	// Sleep waits between samples; nil means time.Sleep (tests inject).
	Sleep func(time.Duration)

	// Committer names the system in Sync's commits; zero means
	// DefaultCommitter.
	Committer Committer
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

// Report is the result of one Sync pass.
type Report struct {
	Outcomes []Outcome
	// Repo is the git work tree the snapshots were committed to; "" when
	// Root is not in a git work tree (snapshots are written, not committed).
	Repo string
	// Commit is the Sync commit made by this pass ("" when nothing changed).
	Commit string
	// CommitErr is set when the commit failed; the written snapshots stay in
	// the work tree and the next pass commits them.
	CommitErr error
}

// RunOnce performs one Sync pass over every sync scope and provider.  A
// failure for one pair does not stop the others; it is reported in its
// Outcome and the previous snapshot stays as it was (never write a shrunken
// or partial view of reality).  Every snapshot written by the pass goes into
// one commit.
func RunOnce(ctx context.Context, opts Options) Report {
	names := make([]string, 0, len(opts.Providers))
	for n := range opts.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	committer := opts.Committer
	if committer == (Committer{}) {
		committer = DefaultCommitter
	}

	var rep Report
	rep.Repo = findRepo(ctx, opts.Root)
	for _, sp := range opts.Intent.SyncScopes() {
		dir := filepath.Join(opts.Root, filepath.FromSlash(sp.TargetPath))
		for _, name := range names {
			o := Outcome{Scope: sp.Scope.String(), Provider: name, Path: filepath.Join(dir, "sync-"+name+".yaml")}
			if rep.Repo != "" {
				if o.Err = checkCommitted(ctx, rep.Repo, o.Path); o.Err != nil {
					rep.Outcomes = append(rep.Outcomes, o)
					continue
				}
			}
			res, err := importer.Run(ctx, importer.Options{
				Providers: map[string]provider.Provider{name: opts.Providers[name]},
				Selector:  sp.Scope,
				Samples:   opts.Samples,
				Interval:  opts.Interval,
				Sleep:     opts.Sleep,
			})
			if err != nil {
				o.Err = err
				rep.Outcomes = append(rep.Outcomes, o)
				continue
			}
			o.Resources = len(res.Resources)
			o.Changed, o.Err = writeSnapshot(o.Path, sp.Scope.String(), name, res.Resources)
			rep.Outcomes = append(rep.Outcomes, o)
		}
	}

	if rep.Repo != "" {
		var paths []string
		var subjects []string
		for _, o := range rep.Outcomes {
			if o.Err == nil {
				paths = append(paths, o.Path)
				subjects = append(subjects, fmt.Sprintf("%s %s", o.Scope, o.Provider))
			}
		}
		rep.Commit, rep.CommitErr = commitSnapshots(ctx, rep.Repo, paths, subjects, committer)
	}
	return rep
}

// flapKey names one field of one resource in a snapshot (the provider is
// the snapshot's).
type flapKey struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	Field string `json:"field"`
}

// snapshotMeta is what a previous snapshot tells the next pass.
type snapshotMeta struct {
	significant string
	flapping    map[flapKey]bool
}

// writeSnapshot writes the snapshot if its meaningful content changed.  It
// refuses to overwrite a file whose body does not match its header hash.
func writeSnapshot(path, scope, providerName string, resources []importer.Resource) (bool, error) {
	prev, err := readSnapshot(path)
	if err != nil {
		return false, err
	}

	// Managed candidates: those seen this pass, plus those remembered from
	// earlier passes for fields still present.
	var flapping []flapKey
	for i := range resources {
		r := &resources[i]
		for f := range r.Fields {
			k := flapKey{Scope: r.ID.Scope.String(), ID: r.ID.Local, Field: f}
			if _, now := r.ManagedCandidates[f]; now || prev.flapping[k] {
				if !now {
					r.ManagedCandidates[f] = []string{r.Fields[f]}
				}
				flapping = append(flapping, k)
			}
		}
	}
	sort.Slice(flapping, func(i, j int) bool {
		a, b := flapping[i], flapping[j]
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Field < b.Field
	})

	significant := significantHash(resources)
	if prev.significant == significant {
		return false, nil
	}

	fl, err := json.Marshal(flapping)
	if err != nil {
		return false, err
	}
	if flapping == nil {
		fl = []byte("[]")
	}
	body := significantPrefix + significant + "\n" + flappingPrefix + string(fl) + "\n" + importer.EmitResourcesYAML(resources)
	sum := sha256.Sum256([]byte(body))

	var b strings.Builder
	b.WriteString(HeaderMarker + "\n")
	b.WriteString("# Sync Mode（§24.5）が現実から自動で書き出したスナップショットです。手で編集しないでください\n")
	b.WriteString("# （次の反映で上書きされるため、編集を検知したら上書きを止めます）。\n")
	b.WriteString("# converge に切り替えるときは、人がレビューして intent/ へ移します（§24.5 cutover）。\n")
	b.WriteString("# managed 候補（観測のたびに値が変わったフィールド）の値の変化だけでは書き直しません。\n")
	fmt.Fprintf(&b, "# scope: %s\n", scope)
	fmt.Fprintf(&b, "# provider: %s\n", providerName)
	b.WriteString(bodyHashPrefix + hex.EncodeToString(sum[:]) + "\n")
	b.WriteString(body)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := writeAtomic(path, []byte(b.String())); err != nil {
		return false, err
	}
	return true, nil
}

// significantHash hashes the snapshot with the values of managed candidates
// left out: the content whose change is worth a commit.
func significantHash(resources []importer.Resource) string {
	sig := make([]importer.Resource, len(resources))
	for i, r := range resources {
		fields := make(map[string]string, len(r.Fields))
		for f, v := range r.Fields {
			if _, flaps := r.ManagedCandidates[f]; flaps {
				v = "(managed 候補)"
			}
			fields[f] = v
		}
		sig[i] = importer.Resource{Provider: r.Provider, ID: r.ID, Fields: fields, Omitted: r.Omitted}
	}
	sum := sha256.Sum256([]byte(importer.EmitResourcesYAML(sig)))
	return hex.EncodeToString(sum[:])
}

// readSnapshot reads the previous snapshot's meta.  A missing file is a zero
// meta; a file without the header or with a body that does not match its hash
// is ErrEditedByHand.
func readSnapshot(path string) (snapshotMeta, error) {
	meta := snapshotMeta{flapping: map[flapKey]bool{}}
	old, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return meta, nil
	}
	if err != nil {
		return meta, err
	}
	oldHash, oldBody, ok := splitSnapshot(old)
	if !ok {
		return meta, fmt.Errorf("%w: %s has no Sync Mode header; move it out of the sync target", ErrEditedByHand, path)
	}
	oldSum := sha256.Sum256(oldBody)
	if hex.EncodeToString(oldSum[:]) != oldHash {
		return meta, fmt.Errorf("%w: %s; restore it or delete it so Sync can rewrite it", ErrEditedByHand, path)
	}
	// The meta lines lead the (hashed) body.  A snapshot written before they
	// existed has none and is simply rewritten once.
	for _, line := range strings.Split(string(oldBody), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		switch {
		case strings.HasPrefix(line, significantPrefix):
			meta.significant = strings.TrimPrefix(line, significantPrefix)
		case strings.HasPrefix(line, flappingPrefix):
			var keys []flapKey
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, flappingPrefix)), &keys); err != nil {
				return meta, fmt.Errorf("%w: %s: unreadable %s", ErrEditedByHand, path, strings.TrimSpace(flappingPrefix))
			}
			for _, k := range keys {
				meta.flapping[k] = true
			}
		}
	}
	return meta, nil
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
