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

//go:build unix

// Package nftguard は nftables provider の決まった道具 sakutei-nft-guard
// （Backlog #191）の中身。対象のホストで root で走るのはこの道具だけにし、
// sudo で許すのも道具だけにする（sh・nft を直接は許さない）。
//
// 設計判断:
//
//   - 道具がやるのは決まった形の引数の次だけ:
//     list <family> <name> json|handles（規則を読む。観測用）・
//     check・apply（標準入力の規則を確かめる・当てる）・
//     arm <秒> [<family>/<name>...]（巻き戻しを予約し token を返す）・
//     disarm <token>（予約を取り消す）・status（道具の版・扱う表・予約の様子を JSON で）。
//   - 扱ってよい表は、ホストの root だけが書ける道具の設定ファイル
//     （既定 /etc/sakutei/nft-guard.json）に書く。呼び手からは渡さない。
//     設定ファイル・そのディレクトリ・nft・状態のディレクトリは、root（か
//     道具を走らせている利用者）の持ち物で group・other が書けないことを確かめる。
//   - check・apply の標準入力は、行ごとに構造として読む: add table・
//     add chain（base chain の { type … hook … priority … ; policy … ; } だけ）・
//     flush chain・delete chain・add rule の 5 つの形だけを受け、どれも許した表の
//     ものでなければならない。ルール文は引用符の外の ';'・'#'・'$'・'\'・制御文字・
//     閉じない括弧と引用符を受けない（1 行に 1 つの命令しか書けない）。flush
//     ruleset・include・define・別の表・table { } の塊は、1 行でもあれば何もせず断る。
//   - 巻き戻しの写しは道具が自分で取る（許した表だけ。呼び手の渡す写しは受けない）。
//     写しと予約はホストの状態のディレクトリ（root だけが読み書きできる）に置き、
//     道具は自分をもう一度起動して（setsid で切り離して）上限の後に写しを nft -f で
//     流す。Sakutei との接続が切れても、Sakutei が居なくなっても戻る。
//     取り消しと流すのは写しのファイルの rename で決着する（先に動かした方が勝つ）。
//   - 自分の起動し直し（_fire）は、予約のディレクトリにだけ置いた nonce を
//     環境変数で受け取ったときだけ動く（呼び手は nonce を読めない）。
//   - 終了コード: 0 成功、ExitNFT（10）は nft が 0 以外で終わった（標準エラーは
//     nft のもの）、ExitRefused（2）は引数・入力を断った（何もしていない）、
//     ExitConfig（3）は設定・状態のディレクトリが使えない、ExitInternal（4）はそれ以外。
package nftguard

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/syslog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 既定と上限。
const (
	defaultNFT        = "/usr/sbin/nft"
	defaultStateDir   = "/run/sakutei-nft-guard"
	defaultMaxSeconds = 600
	maxMaxSeconds     = 3600
	maxInput          = 4 << 20
	maxLine           = 64 << 10
	nonceEnv          = "SAKUTEI_NFT_GUARD_NONCE"
	fireVerb          = "_fire"
)

// Config は道具の設定ファイル（JSON）。
//
//	{
//	  "tables": ["inet/sk"],                   // 必須。扱ってよい表（family/name）
//	  "nft": "/usr/sbin/nft",                  // 省略可。nft の絶対パス
//	  "state_dir": "/run/sakutei-nft-guard",   // 省略可。写しと予約を置く
//	  "max_rollback_seconds": 600              // 省略可。arm の秒の上限（1〜3600）
//	}
type Config struct {
	Tables             []string `json:"tables"`
	NFT                string   `json:"nft"`
	StateDir           string   `json:"state_dir"`
	MaxRollbackSeconds int      `json:"max_rollback_seconds"`

	tables []table
}

type table struct{ family, name string }

func (t table) String() string { return t.family + "/" + t.name }

var (
	families = map[string]bool{"ip": true, "ip6": true, "inet": true, "arp": true, "bridge": true, "netdev": true}
	identRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	tokenRe  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	secsRe   = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	prioRe   = regexp.MustCompile(`^-?[0-9]{1,5}$`)
	hookRe   = regexp.MustCompile(`^[a-z]+$`)
)

func parseTable(s string) (table, bool) {
	f, n, ok := strings.Cut(s, "/")
	if !ok || !families[f] || !identRe.MatchString(n) {
		return table{}, false
	}
	return table{f, n}, true
}

// Options は Main の外から決まるもの（本番は cmd/sakutei-nft-guard が決まった値を渡す）。
type Options struct {
	// ConfigPath は設定ファイル（空なら DefaultConfigPath）。
	ConfigPath string
	// Self は巻き戻しのために起動し直す自分（空なら /proc/self/exe）。
	Self string
	// ChildEnv は起動し直す自分に足す環境変数（テスト用）。
	ChildEnv []string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
}

// exitErr は終了コードつきの失敗。
type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }

func refuse(format string, a ...any) error {
	return &exitErr{ExitRefused, fmt.Sprintf(format, a...)}
}

// Main は道具を 1 回走らせ、終了コードを返す。
func Main(args []string, o Options) int {
	if o.ConfigPath == "" {
		o.ConfigPath = DefaultConfigPath
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	err := run(args, o)
	if err == nil {
		return ExitOK
	}
	var nf *nftFailed
	if errors.As(err, &nf) {
		o.Stderr.Write(nf.stderr)
		return ExitNFT
	}
	code := ExitInternal
	var ee *exitErr
	if errors.As(err, &ee) {
		code = ee.code
	}
	fmt.Fprintf(o.Stderr, "%s: %v\n", Name, err)
	if code == ExitRefused {
		logf(syslog.LOG_WARNING, "refused: %v", err)
	}
	return code
}

func run(args []string, o Options) error {
	if len(args) == 0 {
		return refuse("usage: %s list|check|apply|arm|disarm|status ...", Name)
	}
	cfg, err := loadConfig(o.ConfigPath)
	if err != nil {
		return &exitErr{ExitConfig, err.Error()}
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "list":
		return cmdList(cfg, rest, o)
	case "check", "apply":
		if len(rest) != 0 {
			return refuse("%s takes no arguments (the rules come on the standard input)", verb)
		}
		return cmdApply(cfg, verb == "check", o)
	case "arm":
		return cmdArm(cfg, rest, o)
	case "disarm":
		return cmdDisarm(cfg, rest, o)
	case "status":
		if len(rest) != 0 {
			return refuse("status takes no arguments")
		}
		return cmdStatus(cfg, o)
	case fireVerb:
		return cmdFire(cfg, rest)
	}
	return refuse("unknown command %q (want list, check, apply, arm, disarm or status)", verb)
}

// ─────────────────────────────────────────────────────────────────────────────
// 設定
// ─────────────────────────────────────────────────────────────────────────────

func loadConfig(path string) (*Config, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("config %s: not an absolute path", path)
	}
	if err := checkOwned(filepath.Dir(path), true); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := checkOwned(path, false); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if len(c.Tables) == 0 {
		return nil, fmt.Errorf("config %s: tables: name at least one table (family/name) the tool may touch", path)
	}
	seen := map[table]bool{}
	for _, s := range c.Tables {
		t, ok := parseTable(s)
		if !ok {
			return nil, fmt.Errorf("config %s: tables: %q: want family/name (family: ip, ip6, inet, arp, bridge or netdev)", path, s)
		}
		if seen[t] {
			return nil, fmt.Errorf("config %s: tables: %q is listed twice", path, s)
		}
		seen[t] = true
		c.tables = append(c.tables, t)
	}
	if c.NFT == "" {
		c.NFT = defaultNFT
	}
	if !filepath.IsAbs(c.NFT) {
		return nil, fmt.Errorf("config %s: nft %q: want an absolute path", path, c.NFT)
	}
	if err := checkOwned(c.NFT, false); err != nil {
		return nil, fmt.Errorf("config %s: nft: %w", path, err)
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir
	}
	if !filepath.IsAbs(c.StateDir) {
		return nil, fmt.Errorf("config %s: state_dir %q: want an absolute path", path, c.StateDir)
	}
	if c.MaxRollbackSeconds == 0 {
		c.MaxRollbackSeconds = defaultMaxSeconds
	}
	if c.MaxRollbackSeconds < 1 || c.MaxRollbackSeconds > maxMaxSeconds {
		return nil, fmt.Errorf("config %s: max_rollback_seconds %d: want 1 to %d", path, c.MaxRollbackSeconds, maxMaxSeconds)
	}
	return &c, nil
}

// checkOwned は path が root か自分（実効 uid）の持ち物で、group・other が
// 書けないこと（シンボリックリンクは辿った先）を確かめる。
func checkOwned(path string, dir bool) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if dir != fi.IsDir() {
		if dir {
			return errors.New("not a directory")
		}
		return errors.New("not a regular file")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the owner")
	}
	if st.Uid != 0 && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("owned by uid %d (want root)", st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("writable by group or others (mode %v)", fi.Mode().Perm())
	}
	return nil
}

func (c *Config) allowed(t table) bool {
	for _, a := range c.tables {
		if a == t {
			return true
		}
	}
	return false
}

func (c *Config) tableNames() []string {
	out := make([]string, len(c.tables))
	for i, t := range c.tables {
		out[i] = t.String()
	}
	return out
}

// stateDir は状態のディレクトリを（無ければ 0700 で）作り、持ち主と権限を確かめる。
func (c *Config) stateDir() (string, error) {
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return "", &exitErr{ExitConfig, fmt.Sprintf("state_dir: %v", err)}
	}
	fi, err := os.Lstat(c.StateDir)
	if err != nil {
		return "", &exitErr{ExitConfig, fmt.Sprintf("state_dir: %v", err)}
	}
	if !fi.IsDir() {
		return "", &exitErr{ExitConfig, fmt.Sprintf("state_dir %s: not a directory", c.StateDir)}
	}
	if err := checkOwned(c.StateDir, true); err != nil {
		return "", &exitErr{ExitConfig, fmt.Sprintf("state_dir %s: %v", c.StateDir, err)}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", &exitErr{ExitConfig, fmt.Sprintf("state_dir %s: readable by group or others (mode %v, want 0700)", c.StateDir, fi.Mode().Perm())}
	}
	return c.StateDir, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// nft
// ─────────────────────────────────────────────────────────────────────────────

// nftFailed は nft が 0 以外で終わったこと（標準エラーはそのまま呼び手へ）。
type nftFailed struct {
	code   int
	stderr []byte
}

func (e *nftFailed) Error() string {
	return fmt.Sprintf("nft exited with code %d: %s", e.code, strings.TrimSpace(string(e.stderr)))
}

const nftTimeout = 60 * time.Second

func (c *Config) nft(stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command(c.NFT, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("run %s: %w", c.NFT, err)
	}
	timer := time.AfterFunc(nftTimeout, func() { _ = cmd.Process.Kill() })
	err := cmd.Wait()
	timer.Stop()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.Bytes(), &nftFailed{code: ee.ExitCode(), stderr: stderr.Bytes()}
	}
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", c.NFT, err)
	}
	return stdout.Bytes(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// list・check・apply
// ─────────────────────────────────────────────────────────────────────────────

func (c *Config) tableArg(family, name string) (table, error) {
	t := table{family, name}
	if !families[family] || !identRe.MatchString(name) {
		return table{}, refuse("%q %q is not a table (family name)", family, name)
	}
	if !c.allowed(t) {
		return table{}, refuse("table %s is not one the tool may touch (%s)", t, strings.Join(c.tableNames(), ", "))
	}
	return t, nil
}

func cmdList(c *Config, args []string, o Options) error {
	if len(args) != 3 {
		return refuse("usage: list <family> <name> json|handles")
	}
	t, err := c.tableArg(args[0], args[1])
	if err != nil {
		return err
	}
	var nargs []string
	switch args[2] {
	case "json":
		nargs = []string{"-j", "list", "table", t.family, t.name}
	case "handles":
		nargs = []string{"-a", "-s", "list", "table", t.family, t.name}
	default:
		return refuse("list: format %q: want json or handles", args[2])
	}
	out, err := c.nft(nil, nargs...)
	if err != nil {
		return err
	}
	_, err = o.Stdout.Write(out)
	return err
}

func cmdApply(c *Config, check bool, o Options) error {
	in, err := io.ReadAll(io.LimitReader(o.Stdin, maxInput+1))
	if err != nil {
		return fmt.Errorf("read the rules: %w", err)
	}
	if len(in) > maxInput {
		return refuse("the rules are larger than %d bytes", maxInput)
	}
	if err := c.CheckScript(in); err != nil {
		return err
	}
	args := []string{"-f", "-"}
	if check {
		args = []string{"-c", "-f", "-"}
	}
	out, err := c.nft(in, args...)
	if err != nil {
		return err
	}
	_, err = o.Stdout.Write(out)
	return err
}

// CheckScript は check・apply の標準入力を構造として読み、受けない行が 1 つでも
// あれば（行の番号と理由つきの）ExitRefused の error を返す。
func (c *Config) CheckScript(in []byte) error {
	if !strings.HasSuffix(string(in), "\n") && len(in) > 0 {
		in = append(in, '\n')
	}
	n := 0
	for i, line := range strings.SplitAfter(string(in), "\n") {
		if line == "" {
			continue
		}
		line = strings.TrimSuffix(line, "\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > maxLine {
			return refuse("line %d: longer than %d bytes", i+1, maxLine)
		}
		if err := c.checkLine(line); err != nil {
			return refuse("line %d: %v", i+1, err)
		}
		n++
	}
	if n == 0 {
		return refuse("no rules on the standard input")
	}
	return nil
}

func (c *Config) checkLine(line string) error {
	for _, r := range line {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return errors.New("a control character")
		}
	}
	f := strings.Fields(line)
	if len(f) < 4 {
		return fmt.Errorf("%q: want add table, add chain, flush chain, delete chain or add rule", strings.Join(f, " "))
	}
	what := f[0] + " " + f[1]
	switch what {
	case "add table", "add chain", "flush chain", "delete chain", "add rule":
	default:
		return fmt.Errorf("%q: only add table, add chain, flush chain, delete chain and add rule are taken", what)
	}
	if _, err := c.tableArg(f[2], f[3]); err != nil {
		var ee *exitErr
		if errors.As(err, &ee) {
			return errors.New(ee.msg)
		}
		return err
	}
	if what == "add table" {
		if len(f) != 4 {
			return errors.New("add table takes the family and the name only")
		}
		return nil
	}
	if len(f) < 5 || !identRe.MatchString(f[4]) {
		return fmt.Errorf("%s: want a chain name", what)
	}
	switch what {
	case "flush chain", "delete chain":
		if len(f) != 5 {
			return fmt.Errorf("%s takes the family, the table and the chain only", what)
		}
		return nil
	case "add chain":
		if len(f) == 5 {
			return nil
		}
		return checkBaseSpec(f[5:])
	}
	// add rule: the rest of the line after the chain name.
	if len(f) < 6 {
		return errors.New("add rule: an empty rule")
	}
	rest := line
	for i := 0; i < 5; i++ {
		rest = strings.TrimLeft(rest, " \t")
		j := strings.IndexAny(rest, " \t")
		rest = rest[j:]
	}
	return checkRule(strings.TrimSpace(rest))
}

// checkBaseSpec は add chain の { type T hook H priority P ; policy X ; } を見る。
func checkBaseSpec(f []string) error {
	bad := errors.New("add chain: only a base chain's { type <type> hook <hook> priority <number> ; policy accept|drop ; } is taken")
	if len(f) != 12 || f[0] != "{" || f[1] != "type" || f[3] != "hook" || f[5] != "priority" || f[7] != ";" ||
		f[8] != "policy" || f[10] != ";" || f[11] != "}" {
		return bad
	}
	switch f[2] {
	case "filter", "nat", "route":
	default:
		return bad
	}
	if !hookRe.MatchString(f[4]) || !prioRe.MatchString(f[6]) || (f[9] != "accept" && f[9] != "drop") {
		return bad
	}
	return nil
}

// checkRule はルール文が 1 つの命令に収まる（表の外の命令を持ち込めない）かを見る。
func checkRule(r string) error {
	if r == "" {
		return errors.New("add rule: an empty rule")
	}
	inQuote := false
	depth := 0
	for _, c := range r {
		if c == '\\' {
			return errors.New("add rule: a backslash")
		}
		if c == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch c {
		case ';':
			return errors.New("add rule: ';' outside quotes (it would start another command)")
		case '#':
			return errors.New("add rule: '#' outside quotes")
		case '$':
			return errors.New("add rule: '$' outside quotes (variables are not taken)")
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return errors.New("add rule: an unbalanced '}'")
			}
		}
	}
	if inQuote {
		return errors.New("add rule: an unclosed quote")
	}
	if depth != 0 {
		return errors.New("add rule: an unclosed '{'")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 巻き戻しの予約
// ─────────────────────────────────────────────────────────────────────────────

// 予約のディレクトリ（state_dir/<token>）の中:
//
//	restore.nft  写し（流す前）。取り消すと disarmed.nft、流すと fired.nft に動く
//	fire_at      流す時刻（unix 秒）
//	tables       写しを取った表
//	nonce        起動し直した自分を確かめる
//	pid          起動し直した自分の pid
//	result       流した結果（nft の終了コード）

func cmdArm(c *Config, args []string, o Options) error {
	if len(args) < 1 {
		return refuse("usage: arm <seconds> [<family>/<name>...]")
	}
	if !secsRe.MatchString(args[0]) {
		return refuse("arm: seconds %q: want a whole number from 1 to %d", args[0], c.MaxRollbackSeconds)
	}
	secs, _ := strconv.Atoi(args[0])
	if secs < 1 || secs > c.MaxRollbackSeconds {
		return refuse("arm: seconds %d: want 1 to %d (max_rollback_seconds)", secs, c.MaxRollbackSeconds)
	}
	tables := c.tables
	if len(args) > 1 {
		tables = nil
		seen := map[table]bool{}
		for _, s := range args[1:] {
			t, ok := parseTable(s)
			if !ok {
				return refuse("arm: %q: want family/name", s)
			}
			if !c.allowed(t) {
				return refuse("arm: table %s is not one the tool may touch (%s)", t, strings.Join(c.tableNames(), ", "))
			}
			if seen[t] {
				return refuse("arm: table %s is named twice", t)
			}
			seen[t] = true
			tables = append(tables, t)
		}
	}
	state, err := c.stateDir()
	if err != nil {
		return err
	}
	// The copy of the allowed tables only.
	var restore bytes.Buffer
	for _, t := range tables {
		fmt.Fprintf(&restore, "add table %s %s\ndelete table %s %s\n", t.family, t.name, t.family, t.name)
		out, err := c.nft(nil, "list", "table", t.family, t.name)
		if err != nil {
			var nf *nftFailed
			if errors.As(err, &nf) && strings.Contains(string(nf.stderr), "No such file or directory") {
				continue // no table: the rollback deletes it
			}
			return err
		}
		restore.Write(out)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			restore.WriteByte('\n')
		}
	}
	token, err := randomHex(16)
	if err != nil {
		return err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	dir := filepath.Join(state, token)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	fireAt := time.Now().Add(time.Duration(secs) * time.Second).Unix()
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.String()
	}
	for name, body := range map[string]string{
		"fire_at": strconv.FormatInt(fireAt, 10),
		"tables":  strings.Join(names, " "),
		"nonce":   nonce,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o600); err != nil {
			os.RemoveAll(dir)
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "restore.nft"), restore.Bytes(), 0o600); err != nil {
		os.RemoveAll(dir)
		return err
	}
	self := o.Self
	if self == "" {
		self = "/proc/self/exe"
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	defer devnull.Close()
	child := exec.Command(self, fireVerb, token)
	child.Env = append([]string{nonceEnv + "=" + nonce, "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}, o.ChildEnv...)
	child.Stdin, child.Stdout, child.Stderr = devnull, devnull, devnull
	child.Dir = "/"
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("start the rollback: %w", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600)
	_ = child.Process.Release()
	_, err = fmt.Fprintln(o.Stdout, token)
	return err
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// cmdFire は起動し直した自分: 上限まで眠り、写しが残っていれば（取り消されて
// いなければ）流す。
func cmdFire(c *Config, args []string) error {
	if len(args) != 1 || !tokenRe.MatchString(args[0]) {
		return refuse("not a rollback")
	}
	state, err := c.stateDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(state, args[0])
	nonce := readTrim(filepath.Join(dir, "nonce"))
	if nonce == "" || os.Getenv(nonceEnv) != nonce {
		return refuse("not a rollback started by arm")
	}
	fireAt, err := strconv.ParseInt(readTrim(filepath.Join(dir, "fire_at")), 10, 64)
	if err != nil {
		return refuse("not a rollback: %v", err)
	}
	time.Sleep(time.Until(time.Unix(fireAt, 0)))
	fired := filepath.Join(dir, "fired.nft")
	if err := os.Rename(filepath.Join(dir, "restore.nft"), fired); err != nil {
		return nil // disarmed
	}
	b, err := os.ReadFile(fired)
	if err != nil {
		return err
	}
	rc := 0
	if _, err := c.nft(b, "-f", "-"); err != nil {
		rc = -1
		var nf *nftFailed
		if errors.As(err, &nf) {
			rc = nf.code
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "result"), []byte(strconv.Itoa(rc)+"\n"), 0o600)
	logf(syslog.LOG_WARNING, "the change was not confirmed; restored tables %s as they were (nft exit %d)", readTrim(filepath.Join(dir, "tables")), rc)
	return nil
}

func cmdDisarm(c *Config, args []string, o Options) error {
	if len(args) != 1 || !tokenRe.MatchString(args[0]) {
		return refuse("usage: disarm <token> (32 lowercase hex digits)")
	}
	state, err := c.stateDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(state, args[0])
	if _, err := os.Lstat(dir); err != nil {
		return refuse("disarm: no such rollback %s", args[0])
	}
	if err := os.Rename(filepath.Join(dir, "restore.nft"), filepath.Join(dir, "disarmed.nft")); err != nil {
		os.RemoveAll(dir)
		_, err := fmt.Fprintln(o.Stdout, "fired")
		return err
	}
	if pid, err := strconv.Atoi(readTrim(filepath.Join(dir, "pid"))); err == nil && pid > 1 && isFire(pid, args[0]) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	os.RemoveAll(dir)
	_, err = fmt.Fprintln(o.Stdout, "disarmed")
	return err
}

// isFire は pid が token の巻き戻しの自分かどうか（pid の使い回しで別の
// プロセスを止めないため）。
func isFire(pid int, token string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	return len(args) == 3 && args[1] == fireVerb && args[2] == token
}

func cmdStatus(c *Config, o Options) error {
	st := Status{Tool: Name, Protocol: Protocol, Tables: c.tableNames(), MaxSecs: c.MaxRollbackSeconds, Rollbacks: []Rollback{}}
	state, err := c.stateDir()
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(state)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() || !tokenRe.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(state, e.Name())
		r := Rollback{Token: e.Name(), State: "armed", Tables: strings.Fields(readTrim(filepath.Join(dir, "tables")))}
		r.FireAt, _ = strconv.ParseInt(readTrim(filepath.Join(dir, "fire_at")), 10, 64)
		if _, err := os.Stat(filepath.Join(dir, "restore.nft")); err != nil {
			r.State = "fired"
		}
		st.Rollbacks = append(st.Rollbacks, r)
	}
	sort.Slice(st.Rollbacks, func(i, j int) bool { return st.Rollbacks[i].FireAt < st.Rollbacks[j].FireAt })
	w := bufio.NewWriter(o.Stdout)
	enc := json.NewEncoder(w)
	if err := enc.Encode(st); err != nil {
		return err
	}
	return w.Flush()
}

// logf は syslog へ（届かなければ黙って捨てる）。
func logf(p syslog.Priority, format string, a ...any) {
	w, err := syslog.New(p|syslog.LOG_AUTHPRIV, Name)
	if err != nil {
		return
	}
	defer w.Close()
	_, _ = fmt.Fprintf(w, format, a...)
}
