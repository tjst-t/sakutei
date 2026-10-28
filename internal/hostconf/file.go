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

package hostconf

// file.go — provider "host-file"：ホストの上のふつうのファイル 1 つ。
//
//	resources:
//	  - provider: host-file
//	    id: web1/etc/containers/systemd/app.env     # <ホスト>/<絶対パス>
//	    fields:
//	      content: |                                 # または content_secret: secretref:file/app/env
//	        APP_PORT=8080
//	      mode: "0640"
//	      owner: root
//	      group: app
//
// 決まり:
//
//   - content も content_secret も書かなければ中身は管理しない（無ければ空の
//     ファイルを作り、あとは属性だけを合わせる）。
//   - mode・owner・group を書かなければ、書き換えるときは今のファイルの属性を
//     保つ。新しく作るときは 0644（content_secret なら 0600）、持ち主は書き込む
//     利用者（sudo なら root）。
//   - 書き込みは同じディレクトリの一時ファイルに書いて rename（読み手が半端な
//     中身を見ない）。親のディレクトリは無ければ作る。
//   - シンボリックリンク・ディレクトリなどふつうのファイルでないものは扱わない
//     （観測できない、PermanentError）。
//   - Delete（prune では来ない。Pruneable にしないので）は破壊的：ファイルを消す。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// FileName は provider の名前。
	FileName = "host-file"
	// FileProviderVersion は観測の正規化の版（§10.4）。
	FileProviderVersion = FileName + "-v1"
)

// host-file のフィールド。
const (
	// FieldPath はファイルの絶対パス（identity。書くなら ID のパスと同じ）。
	FieldPath = "path"
	// FieldContent は中身（平文）。観測は "sha256:<hex>"。
	FieldContent = "content"
	// FieldContentSecret は中身の SecretRef。観測は印のファイルの参照。
	FieldContentSecret = "content_secret"
	// FieldMode は 8 進の許可（"0644"）。
	FieldMode = "mode"
	// FieldOwner・FieldGroup は持ち主（名前か数）。
	FieldOwner = "owner"
	FieldGroup = "group"
	// 観測だけのフィールド。
	FieldSize = "size"
	FieldUID  = "uid"
	FieldGID  = "gid"
)

var (
	modeRe = regexp.MustCompile(`^[0-7]{3,4}$`)
	userRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\$?$`)
)

// FileOptions は host-file の設定。
type FileOptions struct {
	// StampDir はホストの上の印の置き場（既定 DefaultStampDir）。
	StampDir string
	// Secrets は content_secret の SecretRef を解決する（nil なら解決できない）。
	Secrets core.SecretResolver
}

// FileProvider は host-file の provider。
type FileProvider struct {
	hosts    Hosts
	stampDir string
	secrets  core.SecretResolver
}

var (
	_ provider.Provider        = (*FileProvider)(nil)
	_ provider.IntentValidator = (*FileProvider)(nil)
	_ provider.PlanRefiner     = (*FileProvider)(nil)
)

// NewFile は hosts のファイルを扱う provider を作る。
func NewFile(hosts Hosts, opts FileOptions) (*FileProvider, error) {
	if len(hosts) == 0 {
		return nil, errors.New("host-file: no host is named (file_hosts)")
	}
	if opts.StampDir == "" {
		opts.StampDir = DefaultStampDir
	}
	if err := cleanAbs(opts.StampDir); err != nil {
		return nil, fmt.Errorf("host-file: stamp_dir: %w", err)
	}
	return &FileProvider{hosts: hosts, stampDir: opts.StampDir, secrets: opts.Secrets}, nil
}

// Schema はフィールドの宣言。
func (p *FileProvider) Schema() provider.Schema {
	s := provider.FieldSpec{Kind: core.FieldKindString}
	computed := provider.FieldSpec{Kind: core.FieldKindString, Computed: true}
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldPath:          {Kind: core.FieldKindString, Identity: true},
		FieldContent:       s,
		FieldContentSecret: {Kind: core.FieldKindSecretRef, Secret: true},
		FieldMode:          s,
		FieldOwner:         s,
		FieldGroup:         s,
		FieldSize:          computed,
		FieldUID:           computed,
		FieldGID:           computed,
	}}
}

// Capabilities は標準階級：List も BulkObserve も無く、消さない（prune しない）。
func (p *FileProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, ProviderVersion: FileProviderVersion}
}

// locate は ID のホストとパス。
func (p *FileProvider) locate(id string) (string, string, error) {
	host, rest, ok := splitID(id)
	if !ok {
		return "", "", fmt.Errorf("want <host>/<absolute path> (got %q)", id)
	}
	if _, ok := p.hosts[host]; !ok {
		return "", "", fmt.Errorf("host %q is not in file_hosts", host)
	}
	file := "/" + rest
	if err := cleanAbs(file); err != nil {
		return "", "", err
	}
	return host, file, nil
}

// stampPath は file の印（SecretRef で書いたときだけ）のパス。
func (p *FileProvider) stampPath(file string) string {
	return p.stampDir + "/file-stamps/" + digest([]byte(file))[:32]
}

// fileReadScript は $1 のファイルを stat し、中身の hash と印 $2 を書く。
const fileReadScript = knownAbsent + `p=$1; s=$2
if [ -L "$p" ]; then echo "file symlink"
elif [ ! -e "$p" ]; then known_absent "$p"; echo "file absent"
elif [ ! -f "$p" ]; then echo "file notfile"
else
  st=$(stat -c '%a %u %g %U %G %s' "$p") || exit 3
  h=$(sha256sum < "$p") || exit 3
  echo "file ok ${h%% *} $st"
fi
if [ -f "$s" ]; then
  l=$(cat "$s") || exit 3
  echo "stamp $l"
elif [ ! -e "$s" ]; then known_absent "$s"
fi
echo end
`

// fileStat はファイルの観測。
type fileStat struct {
	exists                   bool
	hash, mode, uid, gid     string
	user, group, size, stamp string
}

func (p *FileProvider) stat(ctx context.Context, host, file string) (fileStat, error) {
	raw, err := p.hosts[host].Run(ctx, []string{"sh", "-c", fileReadScript, "sh", file, p.stampPath(file)}, nil)
	if err != nil {
		return fileStat{}, err
	}
	var st fileStat
	ended := false
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case len(f) == 1 && f[0] == "end":
			ended = true
		case len(f) == 2 && f[0] == "file" && f[1] == "absent":
		case len(f) == 2 && f[0] == "file":
			return fileStat{}, provider.NewPermanentError(fmt.Errorf("%s is a %s, not a regular file; host-file manages regular files only", file, f[1]))
		case len(f) == 9 && f[0] == "file" && f[1] == "ok":
			m, err := strconv.ParseUint(f[3], 8, 32)
			if err != nil {
				return fileStat{}, fmt.Errorf("unexpected mode %q", f[3])
			}
			st = fileStat{exists: true, hash: f[2], mode: fmt.Sprintf("%04o", m), uid: f[4], gid: f[5],
				user: f[6], group: f[7], size: f[8]}
			if st.user == "UNKNOWN" {
				st.user = st.uid
			}
			if st.group == "UNKNOWN" {
				st.group = st.gid
			}
		case len(f) >= 1 && f[0] == "stamp":
			if len(f) == 3 {
				st.stamp = f[1] + " " + f[2]
			}
		default:
			return fileStat{}, fmt.Errorf("unexpected output line %q", sc.Text())
		}
	}
	if !ended {
		return fileStat{}, errors.New("the output ended before it was complete")
	}
	return st, nil
}

// Read はファイルを観測する（中身は hash だけ）。desired を見ない（ADR-0002）。
func (p *FileProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	host, file, err := p.locate(id)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read %q: %w", FileName, id, err))
	}
	st, err := p.stat(ctx, host, file)
	if err != nil {
		var pe *provider.PermanentError
		if errors.As(err, &pe) {
			return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read %q: %w", FileName, id, err))
		}
		return unknownObs(), readError(FileName, id, err)
	}
	if !st.exists {
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
	}
	state := core.State{
		FieldPath:    str(file),
		FieldContent: str(hashPrefix + st.hash),
		FieldMode:    str(st.mode),
		FieldOwner:   str(st.user),
		FieldGroup:   str(st.group),
		FieldUID:     str(st.uid),
		FieldGID:     str(st.gid),
		FieldSize:    str(st.size),
	}
	// 印の参照は、そのとき書いた中身の hash がいまの中身と同じときだけ観測にする。
	if ref, h, ok := strings.Cut(st.stamp, " "); ok && h == st.hash {
		if r, err := core.ParseSecretRef(ref); err == nil {
			state[FieldContentSecret] = core.TypedValue{Kind: core.FieldKindSecretRef, Value: r}
		}
	}
	return core.Observation{Availability: core.AvailabilityOk, State: state, ObservedAt: time.Now()}, nil
}

// ValidateIntent は書き方の誤りを読み込みの時点で断る。
func (p *FileProvider) ValidateIntent(id string, fields map[string]string) error {
	_, file, err := p.locate(id)
	if err != nil {
		return fmt.Errorf("%s %q: %w", FileName, id, err)
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch k {
		case FieldPath, FieldContent, FieldContentSecret, FieldMode, FieldOwner, FieldGroup:
		default:
			return fmt.Errorf("%s %q: unknown field %q (path | content | content_secret | mode | owner | group)", FileName, id, k)
		}
	}
	if v, ok := fields[FieldPath]; ok && v != file {
		return fmt.Errorf("%s %q: path %q must equal the path in the id (%s)", FileName, id, v, file)
	}
	_, hasContent := fields[FieldContent]
	if _, ok := fields[FieldContentSecret]; ok && hasContent {
		return fmt.Errorf("%s %q: write content or content_secret, not both", FileName, id)
	}
	if len(fields[FieldContent]) > MaxContentBytes {
		return fmt.Errorf("%s %q: content is %d bytes; the limit is %d (it appears in plan as it is)", FileName, id, len(fields[FieldContent]), MaxContentBytes)
	}
	if v, ok := fields[FieldMode]; ok && !modeRe.MatchString(v) {
		return fmt.Errorf("%s %q: mode %q: want octal such as 0644", FileName, id, v)
	}
	for _, k := range []string{FieldOwner, FieldGroup} {
		if v, ok := fields[k]; ok && !userRe.MatchString(v) {
			return fmt.Errorf("%s %q: %s %q: want a name or a number", FileName, id, k, v)
		}
	}
	return nil
}

// RefinePlan は中身を hash で、mode を 8 進の値で、owner・group を名前か数で
// 比べる（§28.4）。I/O はしない。
func (p *FileProvider) RefinePlan(_ string, observed core.State, _ map[string]string, ops []core.Op) ([]core.Op, error) {
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		var fields []core.FieldDiff
		var notes []string
		for _, f := range op.Fields {
			switch f.Field {
			case FieldContent:
				want := hashPrefix + digest([]byte(f.To))
				if want == obsString(observed, FieldContent) {
					continue
				}
				notes = append(notes, fmt.Sprintf("content sha256 %s → %s", short(obsString(observed, FieldContent)), short(want)))
			case FieldMode:
				if sameMode(f.To, obsString(observed, FieldMode)) {
					continue
				}
				notes = append(notes, fmt.Sprintf("mode %s → %s", obsString(observed, FieldMode), f.To))
			case FieldOwner, FieldGroup:
				num := FieldUID
				if f.Field == FieldGroup {
					num = FieldGID
				}
				if f.To == obsString(observed, f.Field) || f.To == obsString(observed, num) {
					continue
				}
				notes = append(notes, fmt.Sprintf("%s %s → %s", f.Field, obsString(observed, f.Field), f.To))
			case FieldContentSecret:
				notes = append(notes, "content_secret "+f.To)
			}
			fields = append(fields, f)
		}
		if len(fields) == 0 {
			continue
		}
		op.Fields = fields
		if len(notes) > 0 {
			op.Diagnostic = fmt.Sprintf("resource %s: %s", op.Target, strings.Join(notes, "; "))
		}
		out = append(out, op)
	}
	return out, nil
}

func sameMode(a, b string) bool {
	x, err1 := strconv.ParseUint(a, 8, 32)
	y, err2 := strconv.ParseUint(b, 8, 32)
	return err1 == nil && err2 == nil && x == y
}

// fileWriteScript は標準入力の中身を $1 に書く（同じディレクトリの一時ファイル
// → rename）。$2〜$4 は mode・owner・group（空なら今のファイルの値、無ければ
// mode は $5）。$6 は印のパス、$7 は印の中身（空なら印を消す）。$8 が keep
// なら、ファイルがすでにあるときは中身に触れず属性だけを合わせる。
const fileWriteScript = `set -e
p=$1; mode=$2; owner=$3; group=$4; dmode=$5; s=$6; stamp=$7
if [ "$8" = keep ] && [ -f "$p" ] && [ ! -L "$p" ]; then
  [ -z "$mode" ] || chmod "$mode" "$p"
  [ -z "$owner" ] || chown "$owner" "$p"
  [ -z "$group" ] || chgrp "$group" "$p"
  exit 0
fi
d=${p%/*}; [ -n "$d" ] || d=/
mkdir -p "$d"
if [ -f "$p" ]; then
  [ -n "$mode" ] || mode=$(stat -c %a "$p")
  [ -n "$owner" ] || owner=$(stat -c %u "$p")
  [ -n "$group" ] || group=$(stat -c %g "$p")
fi
[ -n "$mode" ] || mode=$dmode
t=$(mktemp "$d/.sakutei-XXXXXX")
trap 'rm -f "$t"' EXIT
cat > "$t"
chmod "$mode" "$t"
[ -z "$owner" ] || chown "$owner" "$t"
[ -z "$group" ] || chgrp "$group" "$t"
mv -f "$t" "$p"
trap - EXIT
if [ -n "$stamp" ]; then
  sd=${s%/*}; mkdir -p "$sd"
  printf '%s\n' "$stamp" > "$s.tmp"
  mv -f "$s.tmp" "$s"
elif [ -n "$s" ]; then
  rm -f "$s"
fi
`

// fileAttrScript は $1 の属性だけを合わせる。
const fileAttrScript = `set -e
p=$1
if [ -L "$p" ] || [ ! -f "$p" ]; then echo "$p is not a regular file" >&2; exit 4; fi
[ -z "$2" ] || chmod "$2" "$p"
[ -z "$3" ] || chown "$3" "$p"
[ -z "$4" ] || chgrp "$4" "$p"
`

// fileDeleteScript は $1（ふつうのファイル）と印 $2 を消す。
const fileDeleteScript = `set -e
p=$1
if [ -L "$p" ] || { [ -e "$p" ] && [ ! -f "$p" ]; }; then echo "$p is not a regular file" >&2; exit 4; fi
rm -f "$p" "$2"
`

// Apply は plan を適用する。
func (p *FileProvider) Apply(ctx context.Context, plan core.Plan) error {
	// §20.7 (#185): what this Apply runs uses the write credential.
	ctx = provider.WithWrite(ctx)
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *FileProvider) applyOp(ctx context.Context, op core.Op) error {
	id := op.Target.Local
	host, file, err := p.locate(id)
	if err != nil {
		return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %w", FileName, id, err))
	}
	h := p.hosts[host]
	if op.Kind == core.OpKindDelete {
		if _, err := h.Run(ctx, []string{"sh", "-c", fileDeleteScript, "sh", file, p.stampPath(file)}, nil); err != nil {
			return applyError(FileName, id, err)
		}
		return nil
	}
	want := map[string]string{}
	for _, f := range op.Fields {
		want[f.Field] = f.To
	}
	var data []byte
	stamp, dmode := "", "0644"
	write := false
	switch {
	case want[FieldContentSecret] != "":
		ref, err := core.ParseSecretRef(want[FieldContentSecret])
		if err != nil {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: content_secret is not a SecretRef", FileName, id))
		}
		if p.secrets == nil {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %s: no secret store is configured (secret_store in remote_config or the provider config)", FileName, id, ref))
		}
		v, err := p.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
		if err != nil {
			// 値は文言に出さない（参照だけ）。
			return provider.NewTransientError(fmt.Errorf("%s: Apply %q: resolve %s: cannot read the secret", FileName, id, ref))
		}
		if len(v) > MaxContentBytes {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %s is larger than %d bytes", FileName, id, ref, MaxContentBytes))
		}
		data = []byte(v)
		stamp = ref.String() + " " + digest(data)
		dmode, write = "0600", true
	case hasField(op, FieldContent):
		data, write = []byte(want[FieldContent]), true
	case op.Kind == core.OpKindCreate:
		// 中身を管理しない：無ければ空のファイルを作る。
		data, write = []byte{}, true
	}
	if !write {
		if _, err := h.Run(ctx, []string{"sh", "-c", fileAttrScript, "sh", file, want[FieldMode], want[FieldOwner], want[FieldGroup]}, nil); err != nil {
			return applyError(FileName, id, err)
		}
		return nil
	}
	keep := ""
	if op.Kind == core.OpKindCreate && !hasField(op, FieldContent) && want[FieldContentSecret] == "" {
		keep = "keep" // 中身を管理しないファイルは、すでにあれば中身に触れない
	}
	argv := []string{"sh", "-c", fileWriteScript, "sh", file, want[FieldMode], want[FieldOwner], want[FieldGroup], dmode, p.stampPath(file), stamp, keep}
	if _, err := h.Run(ctx, argv, data); err != nil {
		return applyError(FileName, id, err)
	}
	return nil
}

func hasField(op core.Op, name string) bool {
	for _, f := range op.Fields {
		if f.Field == name {
			return true
		}
	}
	return false
}
