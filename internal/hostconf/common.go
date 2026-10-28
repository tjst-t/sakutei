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

// Package hostconf は離れたホスト（または手元）のファイルと systemd のユニットを
// 宣言どおりに保つ 2 つの provider（Backlog #155、§29.1）:
//
//   - "host-file"（FileProvider）：ID は "<ホスト>/<絶対パス>"（例
//     "web1/etc/app/app.conf" はホスト web1 の /etc/app/app.conf）。中身
//     （content、または SecretRef の content_secret）・mode・owner・group。
//   - "systemd-unit"（UnitProvider）：ID は "<ホスト>/<ユニットのファイル名>"
//     （例 "web1/app.service"、quadlet なら "web1/app.container"。動くユニットは
//     app.service）。元のファイルの中身・望む状態（active / inactive）・（通常の
//     ユニットだけ）enabled・依って立つファイル（restart_on）。
//
// どちらも ssh 越しの実行の層（internal/remote、#130）の上で動く。ホストの名前
// "local" は手元（remote.Local）。
//
// 設計判断:
//
//   - 標準階級（TrueObserve）。List も BulkObserve も持たず、Pruneable にしない：
//     管理の範囲は intent に書いたものだけ。
//   - 中身は観測にも差分にも平文で載せない：観測は中身の sha256（"sha256:<hex>"）
//     と stat。宣言の content とは RefinePlan が hash で比べる（plan の差分の
//     To には宣言の平文が出る。宣言にもともと書いてある値）。SecretRef の中身は
//     ホストの側の印のファイル（<stamp_dir>/file-stamps/）に「書いた SecretRef の
//     参照と、そのときの中身の hash」を残し、いまの中身の hash と同じときだけ
//     その参照を観測として返す（値は Sakutei にもホストの印にも残らない）。
//   - 依って立つファイルを書き換えたらユニットを入れ替える（restart_on）：ユニットを
//     最後に（再）起動したときの元のファイルと依って立つファイルの hash を、
//     ホストの側の印のファイル（<stamp_dir>/unit-stamps/<ユニット>）に残す。
//     Sakutei の中には状態を持たないので、デーモンを起こし直しても、別の道具から
//     見ても同じ（level-triggered）。
//   - 届かない・sudo が通らない・systemctl が答えない・読めないは観測できない
//     （unknown）。absent にしない（§3）。
package hostconf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// LocalHost は手元を指すホストの名前。
	LocalHost = "local"
	// DefaultStampDir はホストの上の印の置き場の既定。
	DefaultStampDir = "/var/lib/sakutei"
	// MaxContentBytes は宣言できる中身（content・SecretRef の値）の大きさの上限。
	// plan の差分に平文で出るので、設定ファイルの大きさにとどめる。
	MaxContentBytes = 256 * 1024
	// hashPrefix は観測の中身の hash の頭。
	hashPrefix = "sha256:"
)

// Hosts はホストの名前 → Executor。
type Hosts map[string]remote.Executor

// splitID は "<ホスト>/<残り>" を分ける。
func splitID(id string) (host, rest string, ok bool) {
	host, rest, ok = strings.Cut(id, "/")
	if !ok || host == "" || rest == "" {
		return "", "", false
	}
	return host, rest, true
}

// cleanAbs は p が絶対パスで、正規形（".." や "//" や末尾の "/" が無い）で、
// "/" そのものでなく、改行と NUL を含まないかどうか。
func cleanAbs(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("%q is not an absolute path", p)
	case p == "/" || path.Clean(p) != p:
		return fmt.Errorf("%q is not a clean absolute path of a file (no .., //, or trailing /)", p)
	case strings.ContainsAny(p, "\x00\n\r"):
		return fmt.Errorf("%q contains a newline or NUL", p)
	}
	return nil
}

// digest は b の sha256 の 16 進。
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// short は hash の頭 12 文字（表示用）。
func short(h string) string {
	h = strings.TrimPrefix(h, hashPrefix)
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// readError は観測の失敗を provider の error にする（どれも unknown）。
func readError(name, id string, err error) error {
	var ce *remote.ConnError
	if errors.As(err, &ce) {
		return remote.ProviderError(fmt.Errorf("%s: Read %q: %w", name, id, err))
	}
	return provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", name, id, err))
}

// applyError は適用の失敗を provider の error にする。
func applyError(name, id string, err error) error {
	var ce *remote.ConnError
	if errors.As(err, &ce) {
		return remote.ProviderError(fmt.Errorf("%s: Apply %q: %w", name, id, err))
	}
	var pe *provider.PermanentError
	if errors.As(err, &pe) {
		return err
	}
	return provider.NewTransientError(fmt.Errorf("%s: Apply %q: %w", name, id, err))
}

// knownAbsent はスクリプトの頭に付ける関数: known_absent <パス> は、無い
// パスの上で最も近い在るディレクトリを辿れる（-x）ときだけ 0。辿れなければ
// 「在るかどうか分からない」を標準エラーに書いて 3 で終わる（読むだけの観測用の
// ユーザーが覗けないディレクトリの中を absent と読まない、§3・§20.7、#185）。
const knownAbsent = `known_absent() {
  d=$1
  while d=$(dirname -- "$d"); [ ! -d "$d" ] && [ "$d" != / ] && [ "$d" != . ]; do :; done
  [ -x "$d" ] && return 0
  echo "cannot tell whether $1 exists: $d cannot be searched by this user" >&2
  exit 3
}
`

// hashScript は引数のパスごとに "<sha256|absent|notfile> <パス>" を書く。
const hashScript = knownAbsent + `for p in "$@"; do
  if [ -f "$p" ]; then
    h=$(sha256sum < "$p") || exit 3
    echo "${h%% *} $p"
  elif [ -e "$p" ]; then echo "notfile $p"
  else known_absent "$p"; echo "absent $p"
  fi
done
echo end
`

// hashFiles はホストの上の paths の中身の hash（無ければ "absent"、ふつうの
// ファイルでなければ "notfile"）を返す。
func hashFiles(ctx context.Context, h remote.Executor, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	argv := append([]string{"sh", "-c", hashScript, "sh"}, paths...)
	raw, err := h.Run(ctx, argv, nil)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != len(paths)+1 || lines[len(lines)-1] != "end" {
		return nil, errors.New("hashing the files: the output ended before it was complete")
	}
	for i, l := range lines[:len(paths)] {
		v, _, ok := strings.Cut(l, " ")
		if !ok {
			return nil, fmt.Errorf("hashing the files: unexpected line %q", l)
		}
		out[paths[i]] = v
	}
	return out, nil
}

// writeStampScript は印のファイル $1 を標準入力の中身で書く（一時ファイル → rename）。
const writeStampScript = `set -e
s=$1; d=${s%/*}
mkdir -p "$d"
t=$(mktemp "$d/.stamp-XXXXXX")
trap 'rm -f "$t"' EXIT
cat > "$t"
chmod 0644 "$t"
mv -f "$t" "$s"
trap - EXIT
`

func writeStamp(ctx context.Context, h remote.Executor, stamp string, body []byte) error {
	if _, err := h.Run(ctx, []string{"sh", "-c", writeStampScript, "sh", stamp}, body); err != nil {
		return fmt.Errorf("write the stamp %s: %w", stamp, err)
	}
	return nil
}

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}

func str(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }

// obsString は observed の field の文字列（無ければ ""）。
func obsString(observed core.State, field string) string {
	tv, ok := observed[field]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v", tv.Value)
}
