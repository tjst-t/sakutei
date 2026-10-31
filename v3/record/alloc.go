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

package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tjst-t/sakutei/v3/core"
)

const allocDir = "alloc"

// ReadAlloc は採番の写し（alloc/<pool>.json、§5.1、N-B13・E-K4）を読む。Record を
// 作らず・ロックも取らない（plan も読む）。無ければ nil。
func ReadAlloc(dir, pool string) (*core.Allocation, error) {
	if dir == "" {
		return nil, nil
	}
	path := filepath.Join(dir, allocDir, pool+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a core.Allocation
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if a.Values == nil {
		a.Values = map[string]int64{}
	}
	if a.Last == nil {
		a.Last = map[string]int64{}
	}
	return &a, nil
}

// WriteAlloc は採番の写しを書く。ロックを持つ処理だけが呼ぶ。
func (r *Record) WriteAlloc(a core.Allocation) error {
	if err := os.MkdirAll(filepath.Join(r.dir, allocDir), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(r.dir, allocDir, a.Pool+".json"), append(data, '\n'))
}
