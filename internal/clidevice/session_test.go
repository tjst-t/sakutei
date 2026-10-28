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

package clidevice

import (
	"strings"
	"testing"
)

func TestScreen(t *testing.T) {
	for _, c := range []struct {
		raw     string
		lines   string
		partial string
	}{
		{"a\r\nb\r\nsw1# ", "a|b", "sw1# "},
		{"a\r\r\nb\n", "a|b", ""},
		{"--More--\x00\r        \rnext\r\n", "next", ""},
		{"abc\bd\r\n", "abd", ""},
		{"\x1b[1;32mgreen\x1b[0m\r\n\x1b=x\r\n", "green|x", ""},
		{"line\a\r\n", "line", ""},
		{"wait\r", "", "wait"},
	} {
		lines, partial := screen([]byte(c.raw))
		if strings.Join(lines, "|") != c.lines || partial != c.partial {
			t.Errorf("screen(%q) = %q, %q; want %q, %q", c.raw, lines, partial, c.lines, c.partial)
		}
	}
}
