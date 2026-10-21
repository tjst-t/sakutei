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
package syncmode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Committer is the identity Sync's commits carry (author and committer).  The
// attribution is the system's (§26.3), never a person's, so Sync's commits do
// not mix with people's intent commits.
type Committer struct {
	Name  string
	Email string
}

// DefaultCommitter is used when no committer is configured.
var DefaultCommitter = Committer{Name: "sakutei-sync", Email: "sakutei-sync@localhost"}

// Trailer marks Sync's commits.  It — not the committer name, which is
// configurable — is how Sync tells its own commits from people's.
const Trailer = "Sakutei-Sync"

// ErrCommittedByHand is returned (wrapped) when the snapshot in HEAD is not
// what Sync last committed: a commit that is not Sync's changed it.
var ErrCommittedByHand = errors.New("syncmode: snapshot was changed by hand in a commit (generated/ refuses direct commits)")

// findRepo returns the top of the git work tree containing root, or "" when
// root is not in one.
func findRepo(ctx context.Context, root string) string {
	top, err := git(ctx, root, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return top
}

// checkCommitted refuses a snapshot whose committed content (HEAD) differs
// from what Sync's last commit of it wrote, or that is committed but was never
// written by Sync.  A snapshot not in HEAD (never committed, or deleted in a
// commit) is fine: Sync writes it afresh.
func checkCommitted(ctx context.Context, root, path string) error {
	rel, err := relPath(root, path)
	if err != nil {
		return err
	}
	head, err := git(ctx, root, nil, "rev-parse", "-q", "--verify", "HEAD:./"+filepath.ToSlash(rel))
	if err != nil || head == "" {
		return nil
	}
	last, err := git(ctx, root, nil, "log", "-1", "--format=%H", "--grep=^"+Trailer+": ", "--", rel)
	if err != nil {
		return err
	}
	if last != "" {
		if want, err := git(ctx, root, nil, "rev-parse", "-q", "--verify", last+":./"+filepath.ToSlash(rel)); err == nil && want == head {
			return nil
		}
	}
	who, _ := git(ctx, root, nil, "log", "-1", "--format=%h by %an <%ae>", "--", rel)
	return fmt.Errorf("%w: %s (last changed in %s); revert it to Sync's last commit, or delete it in a commit so Sync can rewrite it", ErrCommittedByHand, path, who)
}

// commitSnapshots commits those of paths that differ from HEAD — written by
// this pass, or written by an earlier pass whose commit failed — as one
// commit by the system.  Only these paths go in, whatever else is staged.
// subjects name each (scope, provider) and become the commit's trailers.
func commitSnapshots(ctx context.Context, root string, paths, subjects []string, c Committer) (string, error) {
	var rels, trailers []string
	for i, p := range paths {
		rel, err := relPath(root, p)
		if err != nil {
			return "", err
		}
		st, err := git(ctx, root, nil, "status", "--porcelain", "--untracked-files=all", "--", rel)
		if err != nil {
			return "", err
		}
		if st != "" {
			rels = append(rels, rel)
			trailers = append(trailers, Trailer+": "+subjects[i])
		}
	}
	if len(rels) == 0 {
		return "", nil
	}
	if _, err := git(ctx, root, nil, append([]string{"add", "--"}, rels...)...); err != nil {
		return "", err
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + c.Name, "GIT_AUTHOR_EMAIL=" + c.Email,
		"GIT_COMMITTER_NAME=" + c.Name, "GIT_COMMITTER_EMAIL=" + c.Email,
	}
	subject := fmt.Sprintf("sakutei sync: record reality (%d snapshot(s))", len(rels))
	body := "Sync Mode（§24.5）が現実から自動で記録したコミットです。generated/ を手で編集・コミットしないでください。"
	// Unsigned by default (§29.1).  Signing goes here (-S<key>) when added.
	args := []string{"commit", "-q", "--no-gpg-sign", "--only", "-m", subject, "-m", body, "-m", strings.Join(trailers, "\n"), "--"}
	if _, err := git(ctx, root, env, append(args, rels...)...); err != nil {
		return "", err
	}
	return git(ctx, root, nil, "rev-parse", "HEAD")
}

// relPath returns path relative to the work tree top root.  git reports root
// with symbolic links resolved, so path is resolved too before comparing;
// otherwise a path through a link (macOS's /var → /private/var) comes out as
// "../.." and slips past checkCommitted.  path need not exist yet: its
// longest existing ancestor is resolved and the rest appended.
func relPath(root, path string) (string, error) {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir, rest := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			abs = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("syncmode: %s is outside the repository %s", path, root)
	}
	return rel, nil
}

// git runs git in dir and returns its trimmed standard output.  On failure the
// last line of its standard error is attached.
func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
