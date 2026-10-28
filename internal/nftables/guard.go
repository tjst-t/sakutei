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

package nftables

// guard.go — 締め出しの守り（Backlog #173。NETCONF の commit confirmed と同じ
// 考え）。確かめる先の宣言と変更の前の確かめは Backlog #179。
//
// base chain の policy かルールを変える Apply は、自分（Sakutei）の管理の経路を
// 塞ぐかもしれない。そこで live の Apply は、base chain を変えるトランザクション
// を次の順で流す（LiveProvider.applyGuarded）:
//
//  0. 変更の前に、確かめる先（base chain の宣言の confirm、confirm.go。書かな
//     ければ self）のすべてに新しい接続で届くかを確かめる（Guard.Confirm を
//     先ごとに 1 回）。1 つでも届かなければ変えない（一時的な失敗）。前に届かない
//     のに変えると、後の確かめが落ちて戻すだけになるため。
//  1. 変える table の写しを取る（`nft list table`。無ければ「無い」こと）。
//     決まった道具（TableArmer、Backlog #191）は 3 で写しを自分で取る。その前に
//     道具の status で版と扱う表を確かめる（GuardChecker）。
//  2. `nft -c -f -` で検査する。
//  3. 対象のホストの側に、時間の上限（RollbackAfter）の後に写しへ戻す仕掛けを
//     置く（Guard.Arm）。仕掛けはホストの上で時間で動くので、Sakutei との接続が
//     切れても戻る。
//  4. `nft -f -` で 1 つのトランザクションとして流す。
//  5. 確かめる先のすべてに新しい接続で到達を確かめる（Guard.Confirm。self は
//     ssh なら master に相乗りしない新しい ssh の接続、手元なら設定した確かめの
//     宛先への TCP の接続。ssh:// は別の経路の新しい ssh、tcp:// は TCP）。上限の
//     半分まで、まだ届いていない先を張り直す。すべてに届いたときだけ確かめられた
//     とする。
//  6. 確かめられたら仕掛けを取り消す（Guard.Disarm）。確かめられなければ取り
//     消さずに上限を待ち（仕掛けが写しに戻す）、Apply の失敗
//     （provider.RolledBackError）として知らせる。収束のループは同じ差分の Apply
//     をそこで止める（#166 の効かない Apply と同じ扱い、§11.1 Tier3）。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
)

// DefaultRollbackAfter は時間切れの巻き戻しの既定の上限。確かめはその半分まで。
const DefaultRollbackAfter = 30 * time.Second

// ErrRollbackFired は Disarm の前に仕掛けが写しへ戻していたこと。
var ErrRollbackFired = errors.New("the rollback has already restored the previous ruleset")

// Guard は 1 つのホストの締め出しの守りの seam。通常のテストは偽のもの
// （nftfake.Guard）、本物は ExecGuard（sh）と ToolGuard（決まった道具、tool.go、
// Backlog #191）。
type Guard interface {
	// Arm はホストの上に、after の後に restore（`nft -f` に流すスクリプト）を
	// 流す仕掛けを置き、取り消すための token を返す。仕掛けはホストの上で時間で
	// 動く（Sakutei との接続が切れても動く）。
	Arm(ctx context.Context, restore []byte, after time.Duration) (token string, err error)
	// Disarm は token の仕掛けを取り消す。もう流れていたら ErrRollbackFired。
	Disarm(ctx context.Context, token string) error
	// Confirm は新しい接続で to に届くかを確かめる（Backlog #179）。確かめの
	// 手立てが無ければ ErrCannotConfirm を包んで返す。
	Confirm(ctx context.Context, to ConfirmTarget) error
}

// ExecGuard は仕掛けを POSIX の sh で置く Guard。Exec はそのホストでコマンドを
// 走らせる（離れたホストなら remote.SSH。sudo の設定があれば root の sh に
// なる。手元なら remote.Local）。
type ExecGuard struct {
	Exec remote.Executor
	// NFT はそのホストの nft のパス（既定 "nft"）。
	NFT string
	// Probe は self（Sakutei 自身の管理の接続）の新しい接続での確かめ
	// （SSHProbe・TCPProbe）。nil なら self は確かめられない（ErrCannotConfirm）。
	// ssh:// の先は Exec の ProbeVia（remote.SSH）で、tcp:// の先は TCPProbe で
	// 確かめる（Backlog #179）。
	Probe func(ctx context.Context) error
}

// armScript は写し（標準入力）を一時のディレクトリに置き、after 秒の後にそれを
// nft -f で流す仕掛けを、ホストの上で切り離して（setsid・nohup）動かす。
// 標準出力にディレクトリ（token）を書く。$1 は nft、$2 は秒。
const armScript = `set -e
umask 077
d=$(mktemp -d "${TMPDIR:-/tmp}/sakutei-nft-rollback.XXXXXXXX")
cat > "$d/restore.nft"
s=; if command -v setsid >/dev/null 2>&1; then s=setsid; fi
nohup $s sh -c '
sleep "$3"
if mv "$1/restore.nft" "$1/fired.nft" 2>/dev/null; then
  "$2" -f "$1/fired.nft" >"$1/out" 2>&1 && rc=0 || rc=$?
  if command -v logger >/dev/null 2>&1; then logger -t sakutei-nft-rollback "the change was not confirmed; restored the previous ruleset (nft exit $rc)"; fi
fi
rm -rf "$1"' sakutei-nft-rollback "$d" "$1" "$2" </dev/null >/dev/null 2>&1 &
echo "$!" > "$d/pid"
echo "$d"
`

// disarmScript は $1 の仕掛けを取り消す: 写しを動かせたら（まだ流れていない）
// 眠っている仕掛けを止めて "disarmed"、動かせなければ（もう流れた）"fired"。
const disarmScript = `d=$1
case "$d" in */sakutei-nft-rollback.*) ;; *) echo "not a rollback: $d" >&2; exit 2 ;; esac
if mv "$d/restore.nft" "$d/disarmed.nft" 2>/dev/null; then
  p=$(cat "$d/pid" 2>/dev/null || true)
  if [ -n "$p" ]; then kill -- "-$p" 2>/dev/null || kill "$p" 2>/dev/null || true; fi
  rm -rf "$d"
  echo disarmed
else
  echo fired
fi
`

// Arm implements Guard.
func (g ExecGuard) Arm(ctx context.Context, restore []byte, after time.Duration) (string, error) {
	nft := g.NFT
	if nft == "" {
		nft = "nft"
	}
	secs := int((after + time.Second - 1) / time.Second)
	out, err := g.Exec.Run(ctx, []string{"sh", "-c", armScript, "sakutei-nft-arm", nft, strconv.Itoa(secs)}, restore)
	if err != nil {
		return "", fmt.Errorf("arm the rollback: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if !strings.Contains(token, "/sakutei-nft-rollback.") || strings.ContainsAny(token, "\n\t ") {
		return "", fmt.Errorf("arm the rollback: unexpected output %q", token)
	}
	return token, nil
}

// Disarm implements Guard.
func (g ExecGuard) Disarm(ctx context.Context, token string) error {
	out, err := g.Exec.Run(ctx, []string{"sh", "-c", disarmScript, "sakutei-nft-disarm", token}, nil)
	if err != nil {
		return fmt.Errorf("disarm the rollback: %w", err)
	}
	switch strings.TrimSpace(string(out)) {
	case "disarmed":
		return nil
	case "fired":
		return ErrRollbackFired
	}
	return fmt.Errorf("disarm the rollback: unexpected output %q", strings.TrimSpace(string(out)))
}

// Confirm implements Guard.
func (g ExecGuard) Confirm(ctx context.Context, to ConfirmTarget) error {
	switch to.Kind {
	case ConfirmSelf:
		if g.Probe == nil {
			return fmt.Errorf("self: %w (for the local nft set ruleset_confirm_address, or declare confirm: tcp://host:port)", ErrCannotConfirm)
		}
		return g.Probe(ctx)
	case ConfirmSSH:
		v, ok := g.Exec.(interface {
			ProbeVia(ctx context.Context, user, address string, port int) error
		})
		if !ok {
			return fmt.Errorf("%s: %w: host %q has no ssh settings", to, ErrCannotConfirm, g.Exec.Target())
		}
		return v.ProbeVia(ctx, to.User, to.Host, to.Port)
	case ConfirmTCP:
		return TCPProbe(net.JoinHostPort(to.Host, strconv.Itoa(to.Port)))(ctx)
	}
	return fmt.Errorf("%s: %w: unknown kind", to, ErrCannotConfirm)
}

// SSHProbe は新しい ssh の接続での確かめ（remote.SSH の Probe）。Exec が新しい
// 接続を張れなければ error。
func SSHProbe(exec remote.Executor) (func(ctx context.Context) error, error) {
	p, ok := exec.(interface {
		Probe(ctx context.Context) error
	})
	if !ok {
		return nil, fmt.Errorf("host %q cannot open a new connection to confirm reaching it", exec.Target())
	}
	return p.Probe, nil
}

// TCPProbe は address（host:port）への新しい TCP の接続での確かめ（手元の live）。
func TCPProbe(address string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		return c.Close()
	}
}
