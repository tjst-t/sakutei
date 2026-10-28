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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
)

// [AC-174-4-1] WithRealizer で足した変換に、そこを通る許可が reason・tags 付きで
// 渡り、返した resource が intent の resource になる。本体の nftables と同じ
// 名前・二度の登録は起動のエラー。
func TestWithRealizer(t *testing.T) {
	var got []netpolicy.Passing
	acl := netpolicy.RealizerFunc(func(tg netpolicy.Target, allows []netpolicy.Passing) (netpolicy.Output, error) {
		got = allows
		return netpolicy.Output{Resources: []netpolicy.Resource{{Provider: "fake", ID: tg.ID, Fields: map[string]string{"entries": "1"}}}}, nil
	})
	var o options
	WithRealizer("cli-test-acl", acl)(&o)
	if err := registerRealizers(o.realizers); err != nil {
		t.Fatal(err)
	}
	if err := registerRealizers(o.realizers); err == nil {
		t.Error("[AC-174-4-1] the same realizer name twice must be a startup error")
	}
	var dup options
	WithRealizer(netpolicy.DefaultRealizer, acl)(&dup)
	if err := registerRealizers(dup.realizers); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("[AC-174-4-1] the built-in nftables realizer must not be replaced, got %v", err)
	}

	dir := t.TempDir()
	intent := `{"network_policy": {
	  "networks": [{"name": "a", "cidrs": ["10.0.0.0/24"]}, {"name": "b", "cidrs": ["10.0.1.0/24"]}],
	  "allows": [{"from": "a", "to": "b", "proto": "tcp", "ports": [23], "tags": ["exception"], "reason": "古い装置"}],
	  "targets": [{"name": "sw1", "realizer": "cli-test-acl", "kind": "boundary", "networks": ["a", "b"], "id": "sw1-acl"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "np.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Allow.Reason != "古い装置" || !got[0].Allow.HasTag("exception") {
		t.Errorf("[AC-174-4-1] the realizer got %+v", got)
	}
	if len(in.Resources) != 1 || in.Resources[0].ID.Provider != "fake" || in.Resources[0].ID.Local != "sw1-acl" {
		t.Errorf("[AC-174-4-1] resources = %+v", in.Resources)
	}
	rep := netPolicyReport(in.NetworkPolicy)
	if len(rep.Targets) != 1 || rep.Targets[0].Realizer != "cli-test-acl" || rep.Targets[0].Allows[0].Tags[0] != "exception" {
		t.Errorf("[AC-174-4-1] report = %+v", rep)
	}
}
