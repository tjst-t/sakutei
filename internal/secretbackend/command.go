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

package secretbackend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CommandType is the type name of the outside-command backend in
// secret_backends: (§20, #188).
const CommandType = "command"

// Command is the secret backend that asks an outside command (a password
// manager's CLI, a site's own script):
//
//	<command> generation <path>          → prints the generation (a positive integer)
//	<command> value <path> <generation>  → prints the value (one trailing newline is dropped)
//
// A non-zero exit is an error.  stdout is never quoted in an error; stderr
// is quoted (one bounded line) for "generation" only — a value command's
// output stays out of every message.
type Command struct {
	path    string
	timeout time.Duration
}

// NewCommand builds the outside-command backend from its settings:
//
//	command   path of the executable (required)
//	timeout   per call (default 10s)
func NewCommand(settings map[string]string) (*Command, error) {
	const kind = CommandType
	if err := checkKeys(kind, settings, "command", "timeout"); err != nil {
		return nil, err
	}
	c := &Command{path: settings["command"]}
	if c.path == "" {
		return nil, fmt.Errorf("%s: command is required", kind)
	}
	if _, err := exec.LookPath(c.path); err != nil {
		return nil, fmt.Errorf("%s: command %s: %w", kind, c.path, err)
	}
	var err error
	if c.timeout, err = timeoutSetting(kind, settings, 10*time.Second); err != nil {
		return nil, err
	}
	return c, nil
}

// Generation runs "<command> generation <path>".
func (c *Command) Generation(ctx context.Context, path string) (int64, error) {
	out, stderr, err := c.run(ctx, "generation", path)
	if err != nil {
		if s := clip(stderr); s != "" {
			err = fmt.Errorf("%w: %s", err, s)
		}
		return 0, fmt.Errorf("%s: generation of %s: %w", CommandType, path, err)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil || n <= 0 {
		return 0, fmt.Errorf("%s: generation of %s: the command did not print a positive integer", CommandType, path)
	}
	return n, nil
}

// Resolve runs "<command> value <path> <generation>".
func (c *Command) Resolve(ctx context.Context, path string, generation int64) (string, error) {
	out, _, err := c.run(ctx, "value", path, strconv.FormatInt(generation, 10))
	if err != nil {
		return "", fmt.Errorf("%s: value of %s (generation %d): %w", CommandType, path, generation, err)
	}
	out = strings.TrimSuffix(out, "\n")
	return strings.TrimSuffix(out, "\r"), nil
}

func (c *Command) run(ctx context.Context, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limited{w: &stdout, n: 1 << 20}
	cmd.Stderr = &limited{w: &stderr, n: 4 << 10}
	// A child left holding stdout must not outlive the timeout.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", "", fmt.Errorf("timed out after %s", c.timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", stderr.String(), fmt.Errorf("exit status %d", ee.ExitCode())
		}
		return "", "", err
	}
	return stdout.String(), stderr.String(), nil
}

// limited keeps at most n bytes and drops the rest.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := len(p)
		if k > l.n {
			k = l.n
		}
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
