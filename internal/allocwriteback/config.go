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

package allocwriteback

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/syncmode"
)

// Mode is how a written-back allocation reaches the intent (#157).
type Mode string

const (
	// ModeCommit writes the file and commits it straight onto the intent's
	// branch (the work tree's current branch), as the system, riding on Sync
	// Mode's commits (§24.5: committer, thinning).  Outside a git work tree
	// the file is only written.
	ModeCommit Mode = "commit"
	// ModeReview commits the updated file to a separate branch for people to
	// review; taking it into the intent is theirs.  Until the value appears
	// in the intent's file its Apply waits (書き戻しの取り込み待ち).
	ModeReview Mode = "review"
	// ModeFile writes the file only; git is not used.
	ModeFile Mode = "file"
)

const (
	// DefaultPath is where the files go, relative to the repository root
	// (the intent directory's parent, as Sync Mode's target_path): under
	// generated/, one file per pool.
	DefaultPath = "generated/allocations"
	// DefaultBranch is the review branch when none is configured.
	DefaultBranch = "sakutei/allocation-write-back"
	// Trailer marks the write-back's commits ("Sakutei-Allocation: <pool>").
	Trailer = "Sakutei-Allocation"
)

// Config is how the write-back is wired — the intent repository's own
// business, so it is configuration (--alloc-write-back), not intent.  Which
// pools are written back is intent (`pools: [{ write_back: true }]`).
type Config struct {
	// Path is the directory of the files: relative to the repository root,
	// or absolute.  DefaultPath when empty.
	Path string
	// Mode is commit (default), review or file.
	Mode Mode
	// Repo is the git work tree to commit in; the one holding Path when
	// empty.
	Repo string
	// Branch is the review branch (review only); DefaultBranch when empty.
	Branch string
	// Remote, when set, is pushed to after each commit (commit: the current
	// branch; review: the review branch).
	Remote string
	// SSHKey is the private key file the push uses (GIT_SSH_COMMAND).
	SSHKey string
	// Committer names the system in the commits; Sync Mode's
	// DefaultCommitter when zero.
	Committer syncmode.Committer
}

// LoadConfig reads a --alloc-write-back file (JSON / YAML):
//
//	path: generated/allocations     # default
//	mode: commit                    # commit | review | file
//	repo: /srv/intent               # default: the work tree holding path
//	branch: sakutei/allocation-write-back   # review only
//	remote: origin                  # optional: push after each commit
//	ssh_key: /etc/sakutei/deploy-key        # optional, for the push
//	committer_name: sakutei-sync
//	committer_email: sakutei-sync@localhost
//
// A relative repo / ssh_key is taken from the config file's directory.  An
// empty path gives the defaults.
func LoadConfig(path string) (Config, error) {
	var c Config
	if path == "" {
		return c.withDefaults(), nil
	}
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, err
	}
	known := map[string]*string{
		"path": &c.Path, "repo": &c.Repo, "branch": &c.Branch, "remote": &c.Remote, "ssh_key": &c.SSHKey,
		"committer_name": &c.Committer.Name, "committer_email": &c.Committer.Email,
	}
	var mode string
	known["mode"] = &mode
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		dst, ok := known[k]
		if !ok {
			return Config{}, fmt.Errorf("%s: unknown key %q", path, k)
		}
		s, ok := tree[k].(string)
		if !ok {
			return Config{}, fmt.Errorf("%s: %s must be a string", path, k)
		}
		*dst = strings.TrimSpace(s)
	}
	c.Mode = Mode(mode)
	switch c.Mode {
	case "", ModeCommit, ModeReview, ModeFile:
	default:
		return Config{}, fmt.Errorf("%s: mode %q (commit | review | file)", path, mode)
	}
	base := filepath.Dir(path)
	for _, p := range []*string{&c.Repo, &c.SSHKey} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	if (c.Committer.Name == "") != (c.Committer.Email == "") {
		return Config{}, fmt.Errorf("%s: committer_name and committer_email go together", path)
	}
	return c.withDefaults(), nil
}

func (c Config) withDefaults() Config {
	if c.Path == "" {
		c.Path = DefaultPath
	}
	if c.Mode == "" {
		c.Mode = ModeCommit
	}
	if c.Branch == "" {
		c.Branch = DefaultBranch
	}
	if c.Committer == (syncmode.Committer{}) {
		c.Committer = syncmode.DefaultCommitter
	}
	return c
}
