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

package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// confirmProbe は Read の文脈を残す観測だけの provider。
type confirmProbe struct {
	state   core.State
	writing []bool
	got     provider.Confirmer
}

func (p *confirmProbe) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		"name":  {Kind: core.FieldKindString, Identity: true},
		"state": {Kind: core.FieldKindString},
		"seen":  {Kind: core.FieldKindString, Computed: true},
		"key":   {Kind: core.FieldKindString, Secret: true},
	}}
}
func (p *confirmProbe) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, ProviderVersion: "probe-v1"}
}
func (p *confirmProbe) Read(ctx context.Context, id string) (core.Observation, error) {
	p.writing = append(p.writing, provider.Writing(ctx))
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now(), State: p.state}, nil
}
func (p *confirmProbe) Apply(context.Context, core.Plan) error { return nil }
func (p *confirmProbe) UseConfirmer(c provider.Confirmer)      { p.got = c }

func TestConfirmer_ObserveWithObserveCredential(t *testing.T) {
	probe := &confirmProbe{state: core.State{"name": {Kind: core.FieldKindString, Value: "p1"}, "state": {Kind: core.FieldKindString, Value: "ready"}}}
	id := core.NewID("probe", core.NewScope(nil), "p1")
	desired := []diff.DesiredResource{{ID: id, Fields: map[string]string{"state": "ready"}}}
	InstallConfirmer(desired, func(string) provider.Provider { return probe }, nil)
	c := probe.got
	if c == nil {
		t.Fatal("the ConfirmUser got no Confirmer")
	}
	ctx := provider.WithWrite(context.Background())
	if err := c.Observe(ctx, id, map[string]string{"state": "ready"}); err != nil {
		t.Fatalf("as expected: %v", err)
	}
	if err := c.Observe(ctx, id, map[string]string{"state": "busy"}); err == nil || !strings.Contains(err.Error(), "state not as expected") {
		t.Fatalf("not as expected: %v", err)
	}
	if err := c.Observe(ctx, id, nil); err != nil {
		t.Fatalf("present: %v", err)
	}
	for i, w := range probe.writing {
		if w {
			t.Fatalf("read %d was made in the write context", i)
		}
	}
	for expect, want := range map[string]string{"nope": "is not a field", "seen": "computed or managed", "key": "is secret"} {
		if err := c.CheckObserve(id, map[string]string{expect: "x"}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckObserve(%s) = %v", expect, err)
		}
	}
	other := core.NewID("probe", core.NewScope(nil), "p2")
	if err := c.CheckObserve(other, nil); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared: %v", err)
	}
	n := len(probe.writing)
	if err := c.Observe(ctx, other, nil); err == nil || len(probe.writing) != n {
		t.Fatalf("an undeclared resource was read: %v", err)
	}
}

func TestConfirmChecks_Merge(t *testing.T) {
	f := func(ctx context.Context, tg provider.ConfirmTarget) error {
		if provider.Writing(ctx) {
			return errors.New("writing")
		}
		return nil
	}
	np := &netpolicy.Result{Realizations: []netpolicy.Realization{{Target: netpolicy.Target{Name: "t", Realizer: "r"},
		Output: netpolicy.Output{Checks: map[string]provider.ConfirmCheck{"b": f}}}}}
	got, err := ConfirmChecks(map[string]provider.ConfirmCheck{"a": f}, np)
	if err != nil || len(got) != 2 {
		t.Fatalf("merge: %v %v", got, err)
	}
	if _, err := ConfirmChecks(map[string]provider.ConfirmCheck{"b": f}, np); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Fatalf("duplicate: %v", err)
	}
	c := &confirmer{checks: got}
	if err := c.Check(provider.WithWrite(context.Background()), "b", provider.ConfirmTarget{}); err != nil {
		t.Fatalf("the check ran in the write context: %v", err)
	}
}
