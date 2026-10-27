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

package dnszone

// store.go — where the zone file is read and written (Backlog #130).
//
// localStore is the zone file on this machine, exactly as before #130:
// filelock (an in-process lock + an flock on "<zone>.lock"), os.CreateTemp +
// os.Rename.  execStore is the zone file on a host reached through the
// execution layer (internal/remote): the same steps run there as small sh
// scripts, the zone content travelling on stdin/stdout, never in an argument:
//
//   - a read is `cat` under a shared flock(1) on "<zone>.lock" (the same lock
//     file a sakutei running on that host takes, so the two exclude each
//     other);
//   - an Apply holds an exclusive flock on "<zone>.lock" for its whole
//     read-change-write (remote.Executor.Hold: the lock is held by a shell on
//     the host until we close its stdin; if we die, the session closes and the
//     lock goes with it);
//   - a write is mktemp in the zone's directory + cat + mv (atomic rename),
//     keeping the mode and owner of the file it replaces.
//
// A missing zone file is absent only when its directory can be searched (so
// "not there" is certain); otherwise it is an error (unknown ≠ absent).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/filelock"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// zoneStore reads and writes one zone file.
type zoneStore interface {
	// readShared reads the file under the shared lock; os.ErrNotExist if it
	// does not exist.
	readShared(ctx context.Context) ([]byte, error)
	// lock takes the exclusive lock for an Apply's read-change-write.
	lock(ctx context.Context) (func(), error)
	// read reads the file inside lock; os.ErrNotExist if it does not exist.
	read(ctx context.Context) ([]byte, error)
	// write replaces the file atomically (inside lock).
	write(ctx context.Context, data []byte) error
}

// splitLines returns the lines of data without their newline, as reading the
// file line by line does.
func splitLines(data []byte) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan zone file: %w", err)
	}
	return lines, nil
}

// joinLines writes each line with a newline terminator.
func joinLines(lines []string) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ─────────────────────────────────────────────────────────────────────────────
// local
// ─────────────────────────────────────────────────────────────────────────────

// localStore is the zone file on this machine.
type localStore struct{ path string }

func (s localStore) readShared(context.Context) ([]byte, error) {
	defer filelock.RLock(s.path)()
	return os.ReadFile(s.path)
}

func (s localStore) lock(context.Context) (func(), error) { return lockZone(s.path) }

func (s localStore) read(context.Context) ([]byte, error) { return os.ReadFile(s.path) }

// write writes data to the zone file atomically (temp + rename).
func (s localStore) write(_ context.Context, data []byte) error {
	beforeWrite()
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "sakutei-dnszone-*.tmp")
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: create temp: %w", err))
	}
	tmpPath := tmp.Name()
	if _, werr := tmp.Write(data); werr != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: write: %w", werr))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: close temp: %w", err))
	}
	// Atomic rename over the target path.
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: rename: %w", err))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// through the execution layer
// ─────────────────────────────────────────────────────────────────────────────

// exitAbsent is the exit code the read script ends with when the zone file is
// certainly not there.
const exitAbsent = 66

// readScript prints the zone file ($1).  With $2 = shared it first takes a
// shared flock on "$1.lock" (when the lock file can be opened at all, like
// filelock.RLock).
const readScript = `f=$1
if [ "$2" = shared ] && (: >>"$f.lock") 2>/dev/null; then
  exec 9>>"$f.lock"
  flock -s -w 30 9 || { echo "could not lock $f.lock within 30s" >&2; exit 75; }
fi
if [ -e "$f" ] || [ -L "$f" ]; then exec cat -- "$f"; fi
d=$(dirname -- "$f")
if [ -d "$d" ] && [ -x "$d" ]; then exit 66; fi
echo "cannot tell whether $f exists (its directory is missing or cannot be searched)" >&2
exit 1`

// lockScript holds an exclusive flock on "$1.lock" until stdin closes.
const lockScript = `exec 9>>"$1.lock" || exit 75
flock -x -w 30 9 || { echo "could not lock $1.lock within 30s" >&2; exit 75; }
echo ok
exec cat >/dev/null`

// writeScript replaces $1 atomically with stdin, keeping the replaced file's
// mode and owner (a new file is 0644).
const writeScript = `set -e
f=$1
d=$(dirname -- "$f")
t=$(mktemp "$d/.sakutei-dnszone-XXXXXX")
trap 'rm -f -- "$t"' EXIT
cat >"$t"
if [ -e "$f" ]; then
  chmod --reference="$f" -- "$t" 2>/dev/null || chmod 0644 -- "$t"
  chown --reference="$f" -- "$t" 2>/dev/null || :
else
  chmod 0644 -- "$t"
fi
mv -f -- "$t" "$f"
trap - EXIT`

// execStore is the zone file on the host of ex.
type execStore struct {
	ex   remote.Executor
	path string
}

// execLocks serialises the Applies of one process on one host's zone file
// (the remote flock does it between processes).
var execLocks sync.Map // string → *sync.Mutex

func (s execStore) sh(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh", s.path}, args...)
}

func (s execStore) readShared(ctx context.Context) ([]byte, error) {
	return s.cat(ctx, "shared")
}

func (s execStore) read(ctx context.Context) ([]byte, error) { return s.cat(ctx, "") }

func (s execStore) cat(ctx context.Context, mode string) ([]byte, error) {
	out, err := s.ex.Run(ctx, s.sh(readScript, mode), nil)
	var ee *remote.ExitError
	if errors.As(err, &ee) && ee.Code == exitAbsent {
		return nil, os.ErrNotExist
	}
	return out, err
}

func (s execStore) lock(ctx context.Context) (func(), error) {
	v, _ := execLocks.LoadOrStore(s.ex.Target()+"\x00"+s.path, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	release, err := s.ex.Hold(ctx, s.sh(lockScript))
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	return func() {
		_ = release()
		mu.Unlock()
	}, nil
}

func (s execStore) write(ctx context.Context, data []byte) error {
	beforeWrite()
	if _, err := s.ex.Run(ctx, s.sh(writeScript), data); err != nil {
		return remote.ProviderError(fmt.Errorf("dnszone: writeLines: %w", err))
	}
	return nil
}
