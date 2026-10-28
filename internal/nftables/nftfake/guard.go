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

package nftfake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
)

// Guard は締め出しの守り（nftables.Guard、Backlog #173）の偽物。Arm で NFT の
// ruleset の写しを取り、after の後に（Disarm されていなければ）写しに戻す
// （本物はホストの上で写しのスクリプトを nft -f で流す）。
type Guard struct {
	NFT *NFT
	// Unreachable が true を返す間、Confirm は届かない（例: input の policy が
	// drop で ssh を通すルールが無い）。nil なら常に届く。
	Unreachable func(n *NFT) bool
	// ArmErr が nil でなければ Arm はこの error で失敗する。
	ArmErr error

	mu       sync.Mutex
	next     int
	armed    map[string]*time.Timer
	fired    map[string]bool
	restores []string
	// 数え: Arm・Disarm（取り消せた）・Fired（写しに戻した）・Confirm の失敗。
	arms, disarms, fires, unconfirmed int
}

// Arm implements nftables.Guard.
func (g *Guard) Arm(_ context.Context, restore []byte, after time.Duration) (string, error) {
	if g.ArmErr != nil {
		return "", g.ArmErr
	}
	g.NFT.mu.Lock()
	snap := g.NFT.clone()
	g.NFT.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed == nil {
		g.armed, g.fired = map[string]*time.Timer{}, map[string]bool{}
	}
	g.next++
	g.arms++
	token := fmt.Sprintf("/tmp/sakutei-nft-rollback.fake%d", g.next)
	g.restores = append(g.restores, string(restore))
	g.armed[token] = time.AfterFunc(after, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if _, ok := g.armed[token]; !ok {
			return
		}
		delete(g.armed, token)
		g.fired[token] = true
		g.fires++
		g.NFT.mu.Lock()
		g.NFT.tables, g.NFT.nextT = snap.tables, snap.nextT
		g.NFT.mu.Unlock()
	})
	return token, nil
}

// Disarm implements nftables.Guard.
func (g *Guard) Disarm(_ context.Context, token string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fired[token] {
		return nftables.ErrRollbackFired
	}
	t, ok := g.armed[token]
	if !ok {
		return errors.New("nftfake: no such rollback " + token)
	}
	t.Stop()
	delete(g.armed, token)
	g.disarms++
	return nil
}

// Confirm implements nftables.Guard.
func (g *Guard) Confirm(_ context.Context) error {
	if g.Unreachable != nil && g.Unreachable(g.NFT) {
		g.mu.Lock()
		g.unconfirmed++
		g.mu.Unlock()
		return errors.New("nftfake: connection timed out")
	}
	return nil
}

// Counts は Arm・Disarm・写しに戻した回数を返す。
func (g *Guard) Counts() (arms, disarms, fires int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.arms, g.disarms, g.fires
}

// Restores は Arm に渡された写しのスクリプトを返す。
func (g *Guard) Restores() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.restores...)
}

// Armed は取り消されても流れてもいない仕掛けの数。
func (g *Guard) Armed() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.armed)
}

// HasRule は chain に rule（空白をそろえて比べる）があるかどうか（Unreachable の
// 判定に使う）。
func (n *NFT) HasRule(family, tbl, name, rule string) bool {
	want := strings.Join(strings.Fields(rule), " ")
	for _, r := range n.Rules(family, tbl, name) {
		if r == want {
			return true
		}
	}
	return false
}

var _ nftables.Guard = (*Guard)(nil)
