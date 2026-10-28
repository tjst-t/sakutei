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

package provider_test

import (
	"context"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func TestWriteContextAndAccess(t *testing.T) {
	// [AC-185-1-6]
	ctx := context.Background()
	if provider.Writing(ctx) {
		t.Fatal("a plain context is not a write")
	}
	w := provider.WithWrite(ctx)
	if !provider.Writing(w) || !provider.Writing(provider.WithWrite(w)) {
		t.Fatal("WithWrite must mark the context (and stay marked)")
	}
	type k struct{}
	if !provider.Writing(context.WithValue(w, k{}, 1)) {
		t.Fatal("a context derived from a write is a write")
	}
	for v, observeOnly := range map[string]bool{"": false, provider.AccessWrite: false, provider.AccessObserve: true} {
		cfg := map[string]string{provider.ConfigAccess: v}
		if err := provider.CheckAccess(cfg); err != nil {
			t.Errorf("%q: %v", v, err)
		}
		if got := provider.ObserveOnlyRun(cfg); got != observeOnly {
			t.Errorf("ObserveOnlyRun(%q) = %v", v, got)
		}
	}
	if err := provider.CheckAccess(map[string]string{provider.ConfigAccess: "read"}); err == nil {
		t.Error("an unknown access must be refused")
	}
}
