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

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func TestWithConfirmCheck_Register(t *testing.T) {
	saved := baseConfirmChecks
	t.Cleanup(func() { baseConfirmChecks = saved })
	f := func(context.Context, provider.ConfirmTarget) error { return nil }
	var o options
	WithConfirmCheck("route-seen", f)(&o)
	if err := registerConfirmChecks(o.confirmChecks); err != nil || baseConfirmChecks["route-seen"] == nil {
		t.Fatalf("register: %v", err)
	}
	WithConfirmCheck("route-seen", f)(&o)
	if err := registerConfirmChecks(o.confirmChecks); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Fatalf("twice: %v", err)
	}
	if err := registerConfirmChecks([]namedConfirmCheck{{"bad name", f}}); err == nil {
		t.Fatal("a bad name was registered")
	}
	if err := registerConfirmChecks([]namedConfirmCheck{{"nil", nil}}); err == nil {
		t.Fatal("a nil function was registered")
	}
}
