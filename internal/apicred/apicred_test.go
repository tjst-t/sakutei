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

package apicred

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func TestReadInheritsAndOverrides(t *testing.T) {
	// [AC-186-1-1]
	m := map[string]any{
		"endpoint": "https://x", "user": "base", "secret": "secretref:file/base",
		"observe": map[string]any{"user": "ro", "secret": "secretref:file/ro"},
		"write":   map[string]any{"secret": "secretref:file/rw"},
	}
	s, err := Read("c.a", m, "user", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Split || s.Observe["user"] != "ro" || s.Observe["secret"] != "secretref:file/ro" {
		t.Errorf("observe = %v", s.Observe)
	}
	if s.Write["user"] != "base" || s.Write["secret"] != "secretref:file/rw" {
		t.Errorf("write = %v", s.Write)
	}
	if _, ok := s.Observe["endpoint"]; ok {
		t.Error("only the credential keys are taken")
	}
	one, err := Read("c.a", map[string]any{"user": "u"}, "user", "secret")
	if err != nil || one.Split || one.Observe["user"] != "u" || one.Write["user"] != "u" {
		t.Errorf("no split: %+v %v", one, err)
	}
	for _, bad := range []map[string]any{
		{"observe": "ro"},
		{"write": map[string]any{"endpoint": "https://y"}},
	} {
		if _, err := Read("c.a", bad, "user", "secret"); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
}

func TestPairFor(t *testing.T) {
	// [AC-186-1-2]
	p := Pair[string]{Observe: "ro", Write: "rw"}
	ctx := context.Background()
	if c, err := p.For(ctx, "x"); err != nil || c != "ro" {
		t.Errorf("observe: %q %v", c, err)
	}
	if c, err := p.For(provider.WithWrite(ctx), "x"); err != nil || c != "rw" {
		t.Errorf("write: %q %v", c, err)
	}
	p.NoWrite = true
	if c, err := p.For(ctx, "x"); err != nil || c != "ro" {
		t.Errorf("observe-only run, observe: %q %v", c, err)
	}
	_, err := p.For(provider.WithWrite(ctx), "cluster lab")
	if !errors.Is(err, provider.ErrNoWriteAccess) || !strings.Contains(err.Error(), "cluster lab") {
		t.Errorf("observe-only run, write: %v", err)
	}
	if s := Single("one"); s.Observe != "one" || s.Write != "one" {
		t.Errorf("Single = %+v", s)
	}
}
