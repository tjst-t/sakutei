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

package nftables_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/nftables"
)

// fakeGuard は専用ツールの代わりのスクリプト：呼ばれ方と標準入力を log に書く。table は
// 無い（list は nft の「No such file」で終わる）。FAIL_APPLY のファイルがあれば apply が
// 失敗する。
const fakeGuard = `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/log"
case "$1" in
list) echo "Error: No such file or directory" >&2; exit 10 ;;
check) cat >> "$d/log" ;;
apply) cat >> "$d/log"; [ -e "$d/FAIL_APPLY" ] && { echo "Error: boom" >&2; exit 10; } ;;
arm) echo 0123456789abcdef0123456789abcdef ;;
disarm) echo disarmed ;;
rollback) echo "rolled back" ;;
esac
exit 0
`

func liveProvider(t *testing.T) (core.Provider, string) {
	dir := t.TempDir()
	g := filepath.Join(dir, "guard")
	os.WriteFile(g, []byte(fakeGuard), 0o755)
	p, err := nftables.Module(map[string]any{"hosts": map[string]any{"fw1": map[string]any{"local": true, "guard": []any{g}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := core.ID{Type: "fw/chain", Name: "fw1/inet/sk/input"}
	res := core.Resource{ID: id, Fields: core.Fields{"type": "filter", "hook": "input", "priority": int64(0), "policy": "drop",
		"rules": []any{"ct state established,related accept"}}, Apply: &core.ApplyOptions{Transaction: &core.TxnSpec{}}}
	if d := p.(core.Validator).Validate([]core.Resource{res}, []core.Range{{Provider: "fw", Args: map[string]string{"host": "fw1", "tables": "inet/sk"}}}); len(d) > 0 {
		t.Fatal(d)
	}
	return p, dir
}

var liveID = core.ID{Type: "fw/chain", Name: "fw1/inet/sk/input"}

var liveDesired = core.Fields{"type": "filter", "hook": "input", "priority": int64(0), "policy": "drop",
	"rules": []any{"ct state established,related accept"}}

// live の two-phase Apply：check → arm（コピーと時間切れの rollback）→ apply、Commit は
// disarm（E-B3・不変条件 17）。専用ツールが受け付ける形のスクリプトを渡す。
func TestLivePrepareCommit(t *testing.T) {
	p, dir := liveProvider(t)
	if o := observe(t, p, liveID); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
	txn, err := p.(core.Transactor).Prepare(context.Background(), core.WriteContext{}, []core.Change{{ID: liveID, Desired: liveDesired}}, core.TxnOptions{Timeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	want := `check
add table inet sk
add chain inet sk input { type filter hook input priority 0 ; policy drop ; }
flush chain inet sk input
add rule inet sk input ct state established,related accept
arm 2 inet/sk
apply
add table inet sk`
	if !strings.Contains(string(log), want) || !strings.HasSuffix(strings.TrimSpace(string(log)), "disarm 0123456789abcdef0123456789abcdef") {
		t.Fatalf("呼ばれ方:\n%s", log)
	}
}

// apply が失敗すれば予約した rollback をすぐ実行し、確定しない。
func TestLivePrepareApplyFails(t *testing.T) {
	p, dir := liveProvider(t)
	os.WriteFile(filepath.Join(dir, "FAIL_APPLY"), nil, 0o644)
	if _, err := p.(core.Transactor).Prepare(context.Background(), core.WriteContext{}, []core.Change{{ID: liveID, Desired: liveDesired}}, core.TxnOptions{Timeout: 10 * time.Second}); err == nil {
		t.Fatal("失敗しない")
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	if !strings.Contains(string(log), "rollback 0123456789abcdef0123456789abcdef") || strings.Contains(string(log), "disarm") {
		t.Fatalf("呼ばれ方:\n%s", log)
	}
}
