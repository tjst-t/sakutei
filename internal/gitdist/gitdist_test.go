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

package gitdist

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tjst-t/sakutei/pkg/core"
)

func TestLayout(t *testing.T) {
	claim := core.NewScope(map[string]string{"site": "tokyo", "cluster": "prod"})
	paths, err := SparsePaths(claim)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"shards", "intent/platform", "intent/cluster=prod,site=tokyo"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("SparsePaths = %v, want %v", paths, want)
	}
	if p, _ := SecretPrefix(core.NewScope(map[string]string{"site": "a"})); p != "site=a/" {
		t.Errorf("SecretPrefix = %q", p)
	}
	for _, bad := range []map[string]string{nil, {"site": "a/b"}, {"site": ".."}, {"site": ""}} {
		if _, err := ScopeDir(core.NewScope(bad)); err == nil {
			t.Errorf("ScopeDir(%v) accepted", bad)
		}
	}
}

func TestReadShardClaim(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ShardsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, ShardsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tokyo.yaml", "claim:\n  site: tokyo\n")
	write("idle.yaml", "claim:\n")
	write("empty.json", `{"claim": {}}`)
	write("bad.yaml", "claim:\n  site: tokyo\nextra: 1\n")

	sc, err := ReadShardClaim(dir, "tokyo")
	if err != nil || sc.Idle || !sc.Claim.Equal(core.NewScope(map[string]string{"site": "tokyo"})) {
		t.Errorf("tokyo = %+v, %v", sc, err)
	}
	for _, name := range []string{"idle", "empty"} {
		if sc, err := ReadShardClaim(dir, name); err != nil || !sc.Idle {
			t.Errorf("%s = %+v, %v; want idle (never the whole world)", name, sc, err)
		}
	}
	for _, name := range []string{"bad", "missing", "../tokyo"} {
		if _, err := ReadShardClaim(dir, name); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
