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

// tool.go — 決まった道具 sakutei-nft-guard（Backlog #191、internal/nftguard）
// 越しに nft を読み書きし、締め出しの守りを置く。
//
// 設計判断:
//
//   - 対象のホストで root で走るのを道具 1 つにし、sudo で許すのも道具だけに
//     する（sh・nft を直接は許さない。資格が漏れたときの被害を小さくする、
//     #185 と同じ考え）。provider の設定 ruleset_guard_tool に道具のパスを書いた
//     離れたホストだけがこの経路を通る（書かなければ今までどおり nft と sh）。
//   - 読む（Read・List・BulkObserve・Apply の読み戻し）は道具の list、規則を
//     確かめる・当てるのは check・apply（標準入力は今までと同じ nft の
//     スクリプト。道具が行ごとに構造として読み、許した表の外に触れるものは断る）。
//     観測用の資格（#185）でも読めるよう、sudoers に道具の list を許す。
//   - 巻き戻しの写しは道具が自分で取る（ToolGuard は TableArmer。呼び手は
//     写しを渡さない）。base chain を変える前に道具の status で版と扱う表を
//     確かめ、道具が無い・版が合わないなら変えない（一時的な失敗）。
//   - 道具の終了コード: nft の失敗（nftguard.ExitNFT）は nft の失敗として
//     （*ExitError、Code 1）、引数・入力を断った（ExitRefused）は *ExitError の
//     Code 2 として返す（検査で断られたのは PermanentError）。それ以外（道具が
//     無い・sudo が許さない・道具の設定が使えない）は道具が使えない error（一時的）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/nftguard"
	"github.com/tjst-t/sakutei/v2/internal/remote"
)

// TableArmer は写しを自分で取る Guard（道具）。applyGuarded は写しを取らずに
// ArmTables を呼ぶ。
type TableArmer interface {
	ArmTables(ctx context.Context, tables []TableRef, after time.Duration) (token string, err error)
}

// GuardChecker は base chain を変える前に守りが使えるかを確かめる Guard。
// 使えなければ error（ErrToolConfig を包んでいれば直すのは設定、それ以外は一時的）。
type GuardChecker interface {
	CheckGuard(ctx context.Context, tables []TableRef, after time.Duration) error
}

// ErrToolConfig は道具の設定（扱う表・秒の上限）が provider の設定と合わないこと。
var ErrToolConfig = errors.New("the guard tool's config does not cover the change")

// ErrToolUnavailable は道具が無い・走らない・版が合わないこと。
var ErrToolUnavailable = errors.New("the guard tool is not usable")

// runTool は道具を走らせ、終了コードを nft の失敗・断り・使えないに分ける。
func runTool(ctx context.Context, exec remote.Executor, tool string, args []string, stdin []byte) ([]byte, error) {
	out, err := exec.Run(ctx, append([]string{tool}, args...), stdin)
	var ee *remote.ExitError
	if !errors.As(err, &ee) {
		return out, err
	}
	switch ee.Code {
	case nftguard.ExitNFT:
		return out, &ExitError{Code: 1, Stderr: ee.Stderr}
	case nftguard.ExitRefused:
		return out, &ExitError{Code: nftguard.ExitRefused, Stderr: ee.Stderr}
	}
	return out, fmt.Errorf("%w: %s %s: %v", ErrToolUnavailable, tool, args[0], err)
}

// toolArgs は live が nft に渡す引数を道具の引数にする。
func toolArgs(args []string) ([]string, error) {
	switch {
	case len(args) == 5 && args[0] == "-j" && args[1] == "list" && args[2] == "table":
		return []string{"list", args[3], args[4], "json"}, nil
	case len(args) == 6 && args[0] == "-a" && args[1] == "-s" && args[2] == "list" && args[3] == "table":
		return []string{"list", args[4], args[5], "handles"}, nil
	case len(args) == 3 && args[0] == "-c" && args[1] == "-f" && args[2] == "-":
		return []string{"check"}, nil
	case len(args) == 2 && args[0] == "-f" && args[1] == "-":
		return []string{"apply"}, nil
	}
	return nil, fmt.Errorf("nft %s has no counterpart in the guard tool", strings.Join(args, " "))
}

// ToolGuard は決まった道具で仕掛けを置く Guard（TableArmer・GuardChecker）。
type ToolGuard struct {
	Exec remote.Executor
	// Tool はそのホストの道具のパス（既定 nftguard.DefaultPath）。
	Tool string
	// Probe は self の新しい接続での確かめ（ExecGuard と同じ）。
	Probe func(ctx context.Context) error
}

func (g ToolGuard) tool() string {
	if g.Tool == "" {
		return nftguard.DefaultPath
	}
	return g.Tool
}

// Arm implements Guard: 道具は呼び手の写しを受けない（ArmTables を使う）。
func (g ToolGuard) Arm(context.Context, []byte, time.Duration) (string, error) {
	return "", errors.New("the guard tool takes its own copy: use ArmTables")
}

// ArmTables implements TableArmer.
func (g ToolGuard) ArmTables(ctx context.Context, tables []TableRef, after time.Duration) (string, error) {
	args := []string{"arm", strconv.Itoa(rollbackSeconds(after))}
	for _, t := range tables {
		args = append(args, t.String())
	}
	out, err := runTool(ctx, g.Exec, g.tool(), args, nil)
	if err != nil {
		return "", fmt.Errorf("arm the rollback: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" {
		return "", fmt.Errorf("arm the rollback: unexpected output %q", token)
	}
	return token, nil
}

// Disarm implements Guard.
func (g ToolGuard) Disarm(ctx context.Context, token string) error {
	out, err := runTool(ctx, g.Exec, g.tool(), []string{"disarm", token}, nil)
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

// Confirm implements Guard（ExecGuard と同じ確かめ）。
func (g ToolGuard) Confirm(ctx context.Context, to ConfirmTarget) error {
	return ExecGuard{Exec: g.Exec, Probe: g.Probe}.Confirm(ctx, to)
}

// CheckGuard implements GuardChecker: 道具の status で、道具の名前と版、扱う表に
// tables があること、秒の上限が after に足りることを確かめる。
func (g ToolGuard) CheckGuard(ctx context.Context, tables []TableRef, after time.Duration) error {
	out, err := runTool(ctx, g.Exec, g.tool(), []string{"status"}, nil)
	if err != nil {
		if !errors.Is(err, ErrToolUnavailable) {
			err = fmt.Errorf("%w: %v", ErrToolUnavailable, err)
		}
		return err
	}
	var st nftguard.Status
	if err := json.Unmarshal(out, &st); err != nil || st.Tool != nftguard.Name {
		return fmt.Errorf("%w: %s status: not %s", ErrToolUnavailable, g.tool(), nftguard.Name)
	}
	if st.Protocol != nftguard.Protocol {
		return fmt.Errorf("%w: %s speaks protocol %d, this sakutei %d: install the tool of this sakutei's version", ErrToolUnavailable, g.tool(), st.Protocol, nftguard.Protocol)
	}
	have := map[string]bool{}
	for _, t := range st.Tables {
		have[t] = true
	}
	for _, t := range tables {
		if !have[t.String()] {
			return fmt.Errorf("%w: table %s is not in the tool's tables (%s)", ErrToolConfig, t, strings.Join(st.Tables, ", "))
		}
	}
	if secs := rollbackSeconds(after); secs > st.MaxSecs {
		return fmt.Errorf("%w: the rollback after %ds is longer than the tool's max_rollback_seconds %d", ErrToolConfig, secs, st.MaxSecs)
	}
	return nil
}

func rollbackSeconds(after time.Duration) int {
	return int((after + time.Second - 1) / time.Second)
}
