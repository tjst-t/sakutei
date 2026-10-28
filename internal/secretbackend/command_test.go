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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "get-secret")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommand(t *testing.T) {
	c, err := NewCommand(map[string]string{"command": script(t, `
case "$1" in
generation) [ "$2" = app/token ] && echo 7 && exit 0; echo "no such secret $2" >&2; exit 3 ;;
value) [ "$3" = 7 ] && printf '%s\n' "`+value+`"; exit 0 ;;
esac
exit 2
`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if g, err := c.Generation(ctx, "app/token"); err != nil || g != 7 {
		t.Fatalf("Generation = %d, %v", g, err)
	}
	if v, err := c.Resolve(ctx, "app/token", 7); err != nil || v != value {
		t.Fatalf("Resolve = %q, %v", v, err)
	}
	_, err = c.Generation(ctx, "app/other")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "no such secret app/other") {
		t.Errorf("Generation(missing) = %v", err)
	}
}

func TestCommand_NeverQuotesValueOutput(t *testing.T) {
	c, err := NewCommand(map[string]string{"command": script(t, `
case "$1" in
generation) echo "`+value+`" ;;
value) echo "`+value+`"; echo "`+value+`" >&2; exit 1 ;;
esac
`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Generation(ctx, "x"); err == nil || strings.Contains(err.Error(), value) {
		t.Errorf("Generation printing a non-number: %v", err)
	}
	if _, err := c.Resolve(ctx, "x", 1); err == nil || strings.Contains(err.Error(), value) {
		t.Errorf("Resolve failing: %v", err)
	}
}

func TestCommand_Timeout(t *testing.T) {
	c, err := NewCommand(map[string]string{"command": script(t, "exec sleep 5\n"), "timeout": "200ms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generation(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("Generation = %v, want a timeout", err)
	}
}

func TestCommand_Settings(t *testing.T) {
	for name, s := range map[string]map[string]string{
		"no command":  {},
		"not found":   {"command": "/nonexistent/get-secret"},
		"unknown key": {"command": "/bin/sh", "args": "x"},
	} {
		if _, err := NewCommand(s); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
