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
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/alloc"
	"github.com/tjst-t/sakutei/v3/record"
)

// allocLockWait は alloc release が Record のロックを待つ時間（daemon の cycle が
// 終わるのを待つ）。
const allocLockWait = 30 * time.Second

// runAllocRelease は採番の値を人が返す（N-B13・N-J4）。intent のファイルと Record の
// 写しを同じロックの下で書き換えるので、ほかの人の判断のサブコマンド（ファイルを
// 書くだけ）と違い、Record のロックを取る。reuse: false のプールは返した値も二度と
// 払い出さない（不変条件 11）。
func runAllocRelease(ctx context.Context, args []string, stdout, stderr io.Writer, o *options) int {
	const name = "alloc release"
	h := newHumanFlags(name, stderr)
	intentDir := h.fs.String("intent", "", "intent のディレクトリ")
	reason := h.fs.String("reason", "", "返却の理由")
	pos, ok := h.parse(name, args, 2, stderr)
	if !ok {
		return ExitError
	}
	if *intentDir == "" || strings.TrimSpace(*reason) == "" {
		fmt.Fprintf(stderr, "sakutei %s: --intent と --reason を指定する\n", name)
		return ExitError
	}
	who, ok := h.who(name, stderr)
	if !ok {
		return ExitError
	}
	v, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: 値 %q が整数でない（プールの番号を指定する）\n", name, pos[1])
		return ExitError
	}
	rec, err := record.Open(*h.recd)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return ExitError
	}
	deadline := time.Now().Add(allocLockWait)
	var release func()
	for {
		rel, held, err := rec.Lock()
		if err != nil {
			fmt.Fprintf(stderr, "sakutei %s: Record のロック: %v\n", name, err)
			return ExitError
		}
		if held {
			release = rel
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "sakutei %s: Record のロックを %s 待っても取れない（cycle が長く続いている）。時間をおいて再実行する\n", name, allocLockWait)
			return ExitError
		}
		select {
		case <-ctx.Done():
			return ExitError
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer release()
	audit, err := rec.Audit()
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: 監査を開けない: %v\n", name, err)
		return ExitError
	}
	keys, err := alloc.Release(*intentDir, rec, pos[0], v)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return ExitError
	}
	now := o.clock.Now()
	for _, k := range keys {
		res, field := splitKey(k)
		if _, err := audit.Append(record.Entry{At: now, Kind: "alloc.release", Resource: res, Fields: []string{field}, Pool: pos[0],
			Value: &v, By: who, Reason: *reason, Result: "ok"}); err != nil {
			fmt.Fprintf(stderr, "sakutei %s: 値は返したが監査に書けない: %v\n", name, err)
			return ExitError
		}
	}
	fmt.Fprintf(stdout, "プール %s の値 %d を返した（%s）\n", pos[0], v, strings.Join(keys, ", "))
	return ExitOK
}

func splitKey(key string) (res, field string) {
	i := strings.LastIndex(key, "#")
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}
