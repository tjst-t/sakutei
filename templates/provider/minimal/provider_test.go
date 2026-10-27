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

package goldenmin

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// fakeClient は Client のインメモリ実装（契約テスト用の対象の代役）。
type fakeClient struct {
	mu   sync.Mutex
	objs map[string]map[string]string
	down bool
}

var errUnreachable = errors.New("target unreachable")

func (c *fakeClient) Exists(_ context.Context, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return false, errUnreachable
	}
	_, ok := c.objs[name]
	return ok, nil
}

func (c *fakeClient) Ensure(_ context.Context, name string, fields map[string]string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return false, errUnreachable
	}
	cur, ok := c.objs[name]
	if !ok {
		cur = map[string]string{}
		c.objs[name] = cur
	}
	changed := !ok
	for k, v := range fields {
		if cur[k] != v {
			cur[k], changed = v, true
		}
	}
	return changed, nil
}

func (c *fakeClient) Remove(_ context.Context, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return false, errUnreachable
	}
	_, ok := c.objs[name]
	delete(c.objs, name)
	return ok, nil
}

func TestValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(New(&fakeClient{})); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
}

// 付属の契約テスト（§28.6）。最小階級なので List / 等価性テストは無い。
func TestContract(t *testing.T) {
	c := &fakeClient{objs: map[string]map[string]string{}}
	ct := sdk.ContractTester{
		Provider: New(c),
		Scope:    core.NewScope(nil),
		MakeUnreachable: func() (func(), error) {
			c.mu.Lock()
			c.down = true
			c.mu.Unlock()
			return func() { c.mu.Lock(); c.down = false; c.mu.Unlock() }, nil
		},
	}
	ct.RunAll(t)
}
