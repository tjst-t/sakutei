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

package goldenfull

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// errUnreachable は fakeClient が「対象に届かない」ときに返すエラー。
var errUnreachable = errors.New("target unreachable")

// fakeClient は Client のインメモリ実装（契約テスト用の対象の代役）。
// Client を実機用に書き換えても、契約テストはこの fake の上で回し続ける。
type fakeClient struct {
	mu      sync.Mutex
	records map[string]Record
	down    bool
}

func newFakeClient() *fakeClient { return &fakeClient{records: map[string]Record{}} }

// setDown は到達不能を切り替える（三値の正しさの検査に使う）。
func (c *fakeClient) setDown(down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = down
}

func (c *fakeClient) Get(_ context.Context, name string) (Record, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return Record{}, false, errUnreachable
	}
	r, ok := c.records[name]
	return r, ok, nil
}

func (c *fakeClient) Put(_ context.Context, rec Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return errUnreachable
	}
	c.records[rec.Name] = rec
	return nil
}

func (c *fakeClient) Delete(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return errUnreachable
	}
	delete(c.records, name)
	return nil
}

func (c *fakeClient) ListAll(_ context.Context) ([]Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, errUnreachable
	}
	out := make([]Record, 0, len(c.records))
	for _, r := range c.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
