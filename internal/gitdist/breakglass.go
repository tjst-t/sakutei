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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
)

// 書込 break-glass（§25.1 (b)）。
//
// git の正（中央）に届かない間だけ、拠点のローカルミラー（§25.2）へ署名付きコミットを
// 書ける。使用は必ず S4 に記録され（記録できなければ書かない）、BreakGlassWriteUsed
// Event になる。ミラーは session を開いている間は中央を追わず（緊急書込を中央の古い
// 先端で巻き戻さない）、中央が戻ると合流用ブランチ breakglass/<session> を中央へ push
// して PR の下書きを作り、中央の先端が break-glass コミットを祖先に含んだら（merge
// commit / fast-forward で取り込まれたら）session を閉じて鍵を失効させる。
//
// 署名は ed25519（stdlib）。コミットの tree・parent・メッセージ（署名行を除く）を
// 署名し、メッセージ末尾の trailer に置く:
//
//	Sakutei-Break-Glass: <session>
//	Sakutei-Break-Glass-Actor: <actor>
//	Sakutei-Break-Glass-Key: <鍵の指紋>
//	Sakutei-Break-Glass-Signature: <base64 署名>

const (
	trailerSession   = "Sakutei-Break-Glass"
	trailerActor     = "Sakutei-Break-Glass-Actor"
	trailerKey       = "Sakutei-Break-Glass-Key"
	trailerSignature = "Sakutei-Break-Glass-Signature"

	signedPayloadHeader = "sakutei-break-glass-v1\n"
	privateKeyTag       = "sakutei-break-glass-ed25519"
	publicKeyTag        = "sakutei-break-glass-ed25519-pub"

	// SessionFile は session の印（ミラーの bare リポジトリ直下）。これがある間ミラーは
	// 中央を追わない。
	SessionFile = "sakutei-breakglass.json"
	// MergeBranchPrefix は事後合流のブランチ名の接頭辞（breakglass/<session>）。
	MergeBranchPrefix = "breakglass/"
	// upstreamRef は session 中に中央の先端を取っておく ref（ミラーの公開 ref の外）。
	upstreamRef = "refs/sakutei/upstream/"
)

// ── 鍵 ──────────────────────────────────────────────────────────────────────

// GenerateBreakGlassKey は break-glass 書込鍵を作り、out（0600）と out.pub に書く。
// 鍵は平時にオフライン相当の場所へ保管しておく（§20 Break Glass 原則）。
func GenerateBreakGlassKey(out string) (fingerprint string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("breakglass: %w", err)
	}
	if err := writeFileAtomic(out, []byte(privateKeyTag+" "+base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := writeFileAtomic(out+".pub", []byte(publicKeyTag+" "+base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return "", err
	}
	return KeyFingerprint(pub), nil
}

// LoadBreakGlassKey は GenerateBreakGlassKey が書いた秘密鍵を読む。
func LoadBreakGlassKey(path string) (ed25519.PrivateKey, error) {
	raw, err := readTagged(path, privateKeyTag)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.SeedSize {
		return nil, fmt.Errorf("breakglass: %s: not an ed25519 seed", path)
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// LoadBreakGlassPublicKey は公開鍵（.pub）を読む。
func LoadBreakGlassPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := readTagged(path, publicKeyTag)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("breakglass: %s: not an ed25519 public key", path)
	}
	return ed25519.PublicKey(raw), nil
}

func readTagged(path, tag string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("breakglass: %w", err)
	}
	f := strings.Fields(string(data))
	if len(f) != 2 || f[0] != tag {
		return nil, fmt.Errorf("breakglass: %s is not a %s file", path, tag)
	}
	raw, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return nil, fmt.Errorf("breakglass: %s: %w", path, err)
	}
	return raw, nil
}

// KeyFingerprint は公開鍵の指紋（"sha256:" + 先頭 16 バイトの hex）。
func KeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

// ── 「git 到達不能」の判定（§29.1） ────────────────────────────────────────────

// Probe は「中央 git に到達不能」の判定基準（§29.1 で決めた事項）。
//
// 中央（ミラーの upstream）への `git ls-remote` を、1 回 Timeout 以内で、Interval を
// あけて Attempts 回続けて試み、**すべて**失敗したときだけ到達不能とみなす。1 回でも
// 届けば break-glass は使えない（通常の PR 経路で書く）。判定するのは中央であって
// ミラーではない（ミラーに届いても正には届かない）。認証の失敗も「届かない」に数える
// （被災で資格の発行元が落ちた場合も含むため）。
type Probe struct {
	Attempts int
	Interval time.Duration
	Timeout  time.Duration
	// Log は 1 回ごとの判定の出力先（nil なら捨てる）。既定では判定に 20 秒以上かかるので、
	// 運用者に何を待っているかを見せる（試用 docs/trial/dr-mode で見つけた）。
	Log io.Writer
}

// 判定基準の既定値と下限。
const (
	DefaultProbeAttempts = 3
	MinProbeAttempts     = 3
	DefaultProbeInterval = 10 * time.Second
	DefaultProbeTimeout  = 10 * time.Second
)

// CheckUnreachable は Probe の基準で upstream が到達不能なら nil を返す。
func CheckUnreachable(ctx context.Context, upstream string, p Probe) error {
	if p.Attempts < MinProbeAttempts {
		return fmt.Errorf("breakglass: at least %d probe attempts are required to call the central git unreachable", MinProbeAttempts)
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultProbeTimeout
	}
	for i := 0; i < p.Attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.Interval):
			}
		}
		pctx, cancel := context.WithTimeout(ctx, p.Timeout)
		_, err := runGit(pctx, "", "ls-remote", "--heads", upstream)
		timedOut := pctx.Err() == context.DeadlineExceeded
		cancel()
		if p.Log != nil {
			switch {
			case err == nil:
				fmt.Fprintf(p.Log, "sakutei breakglass: probe %d/%d: the central git %s answered\n", i+1, p.Attempts, upstream)
			case timedOut:
				fmt.Fprintf(p.Log, "sakutei breakglass: probe %d/%d: no answer from %s within %s\n", i+1, p.Attempts, upstream, p.Timeout)
			default:
				fmt.Fprintf(p.Log, "sakutei breakglass: probe %d/%d: %s unreachable: %v\n", i+1, p.Attempts, upstream, err)
			}
		}
		if err == nil {
			return fmt.Errorf("breakglass: the central git %s is reachable (probe %d/%d answered); write through the normal PR flow", upstream, i+1, p.Attempts)
		}
	}
	return nil
}

// ── session（ミラーの印） ─────────────────────────────────────────────────────

// Session は 1 回の到達不能期間の break-glass 書込のまとまり。ミラーに置く。
type Session struct {
	ID       string    `json:"id"`
	Branch   string    `json:"branch"`
	Upstream string    `json:"upstream"`
	Base     string    `json:"base"` // session を開いたときのミラーの先端（中央の最後の先端）
	Commits  []string  `json:"commits,omitempty"`
	Keys     []string  `json:"keys,omitempty"`
	AuditLog string    `json:"audit_log"`
	Opened   time.Time `json:"opened"`
	// MergeBranch・MergeTip・PRDraft は事後合流の起票の記録（未起票なら空）。
	MergeBranch string `json:"merge_branch,omitempty"`
	MergeTip    string `json:"merge_tip,omitempty"`
	PRDraft     string `json:"pr_draft,omitempty"`
}

// ReadSession はミラーの session を返す（無ければ nil）。
func ReadSession(mirror string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(mirror, SessionFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("breakglass: %w", err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("breakglass: %s: %w", SessionFile, err)
	}
	return &s, nil
}

func writeSession(mirror string, s *Session) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("breakglass: %w", err)
	}
	return writeFileAtomic(filepath.Join(mirror, SessionFile), append(data, '\n'), 0o644)
}

// ── 書込 ────────────────────────────────────────────────────────────────────

// Write は 1 回の break-glass 書込。Repo はローカルミラーの clone（origin = ミラー）で、
// その作業ツリーの変更をすべて 1 コミットにする。
type Write struct {
	Repo   string
	Branch string
	Key    ed25519.PrivateKey
	Actor  string
	Reason string
	// AuditLog は S4 監査ログ（必須）。記録できなければ書かない。
	AuditLog string
	Probe    Probe
	Now      func() time.Time
}

// WriteResult は break-glass 書込の結果。
type WriteResult struct {
	Commit   string
	Session  string
	Mirror   string
	Upstream string
	Key      string
}

// Commit は判定（中央に到達不能）→ 鍵の失効確認 → 署名付きコミット → S4 記録 →
// session の印 → ミラーへ push の順に進める。どこかで止まればミラーは変わらない
// （S4 の記録の後で止まったときは、使用の記録だけが残る）。
func (w *Write) Commit(ctx context.Context) (*WriteResult, error) {
	if w.Actor == "" || w.Reason == "" {
		return nil, errors.New("breakglass: an actor and a reason are required")
	}
	if w.AuditLog == "" {
		return nil, errors.New("breakglass: an S4 audit log is required (a break-glass write is never silent)")
	}
	if w.Key == nil {
		return nil, errors.New("breakglass: a break-glass key is required")
	}
	branch := w.Branch
	if branch == "" {
		branch = DefaultBranch
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	origin, err := runGit(ctx, w.Repo, "remote", "get-url", "origin")
	if err != nil {
		return nil, fmt.Errorf("breakglass: %s has no origin (it must be a clone of the local mirror): %w", w.Repo, err)
	}
	mirror, err := localPath(origin)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		return nil, fmt.Errorf("breakglass: origin %s is not a local mirror repository", origin)
	}
	upstream, err := runGit(ctx, mirror, "config", "--get", "remote.origin.url")
	if err != nil || upstream == "" {
		return nil, fmt.Errorf("breakglass: the mirror %s has no upstream (central git)", mirror)
	}
	fp := KeyFingerprint(w.Key.Public().(ed25519.PublicKey))
	if retired, err := keyRetired(w.AuditLog, fp); err != nil {
		return nil, err
	} else if retired {
		return nil, fmt.Errorf("breakglass: key %s was retired after its session merged back; rotate to a new break-glass key", fp)
	}

	if err := CheckUnreachable(ctx, upstream, w.Probe); err != nil {
		return nil, err
	}

	tip, err := runGit(ctx, mirror, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("breakglass: the mirror has no branch %s", branch)
	}
	head, err := runGit(ctx, w.Repo, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil || head != tip {
		return nil, fmt.Errorf("breakglass: %s is not at the mirror's %s (%s); pull it first so the write fast-forwards", w.Repo, branch, shortID(tip))
	}
	if _, err := runGit(ctx, w.Repo, "add", "-A"); err != nil {
		return nil, err
	}
	tree, err := runGit(ctx, w.Repo, "write-tree")
	if err != nil {
		return nil, err
	}
	if old, _ := runGit(ctx, w.Repo, "rev-parse", "HEAD^{tree}"); old == tree {
		return nil, errors.New("breakglass: nothing to commit")
	}

	sess, err := ReadSession(mirror)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		t := now().UTC()
		sess = &Session{
			ID:       "bg-" + t.Format("20060102T150405Z") + "-" + strings.TrimPrefix(fp, "sha256:")[:8],
			Branch:   branch,
			Upstream: upstream,
			Base:     tip,
			AuditLog: absPath(w.AuditLog),
			Opened:   t,
		}
	} else if sess.Branch != branch {
		return nil, fmt.Errorf("breakglass: session %s is open on branch %s", sess.ID, sess.Branch)
	}

	msg := fmt.Sprintf("break-glass: %s\n\n%s: %s\n%s: %s\n%s: %s\n",
		firstLine(w.Reason), trailerSession, sess.ID, trailerActor, w.Actor, trailerKey, fp)
	if rest := strings.TrimSpace(strings.TrimPrefix(w.Reason, firstLine(w.Reason))); rest != "" {
		msg = fmt.Sprintf("break-glass: %s\n\n%s\n\n%s: %s\n%s: %s\n%s: %s\n",
			firstLine(w.Reason), rest, trailerSession, sess.ID, trailerActor, w.Actor, trailerKey, fp)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(w.Key, signedPayload(tree, head, msg)))
	msg += trailerSignature + ": " + sig + "\n"
	commit, err := commitTree(ctx, w.Repo, tree, head, msg, w.Actor, now())
	if err != nil {
		return nil, err
	}

	// 使用の記録が先（記録できなければ書かない）。
	if err := audit.NewLog(w.AuditLog).Append(audit.Record{
		Time: now(), Kind: audit.KindBreakGlassWrite, Actor: w.Actor, Signature: sig,
		Session: sess.ID, Commit: commit, Key: fp,
		Detail: fmt.Sprintf("mirror=%s upstream=%s branch=%s base=%s reason=%s", mirror, upstream, branch, sess.Base, firstLine(w.Reason)),
	}); err != nil {
		return nil, fmt.Errorf("breakglass: refusing to write: %w", err)
	}
	// session の印をミラーに置いてから push する（push の後でミラーが中央を追って
	// 緊急書込を上書きすることがないように）。
	if err := writeSession(mirror, sess); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, w.Repo, "push", "--quiet", "origin", commit+":refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("breakglass: push to the local mirror: %w", err)
	}
	sess.Commits = append(sess.Commits, commit)
	if !containsString(sess.Keys, fp) {
		sess.Keys = append(sess.Keys, fp)
	}
	if err := writeSession(mirror, sess); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, w.Repo, "update-ref", "HEAD", commit); err != nil {
		return nil, err
	}
	return &WriteResult{Commit: commit, Session: sess.ID, Mirror: mirror, Upstream: upstream, Key: fp}, nil
}

func signedPayload(tree, parent, msg string) []byte {
	return []byte(signedPayloadHeader + "tree " + tree + "\nparent " + parent + "\n\n" + msg)
}

func commitTree(ctx context.Context, repo, tree, parent, msg, actor string, at time.Time) (string, error) {
	f, err := os.CreateTemp("", "sakutei-breakglass-msg-*")
	if err != nil {
		return "", fmt.Errorf("breakglass: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(msg); err != nil {
		f.Close()
		return "", fmt.Errorf("breakglass: %w", err)
	}
	f.Close()
	date := fmt.Sprintf("%d +0000", at.Unix())
	return runGitEnv(ctx, repo, []string{
		"GIT_AUTHOR_NAME=" + actor, "GIT_AUTHOR_EMAIL=break-glass@sakutei.invalid", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + actor, "GIT_COMMITTER_EMAIL=break-glass@sakutei.invalid", "GIT_COMMITTER_DATE=" + date,
	}, "commit-tree", tree, "-p", parent, "--no-gpg-sign", "-F", f.Name())
}

// ── 検証・検出 ────────────────────────────────────────────────────────────────

// Signed は検証済みの break-glass コミット。
type Signed struct {
	Commit, Session, Actor, Key string
}

// VerifyBreakGlassCommit は commit が break-glass コミットで、keys のいずれかの署名が
// 正しいことを確かめる。
func VerifyBreakGlassCommit(ctx context.Context, repo, commit string, keys []ed25519.PublicKey) (*Signed, error) {
	raw, err := runGitRaw(ctx, repo, "cat-file", "commit", commit)
	if err != nil {
		return nil, err
	}
	hdr, msg, ok := strings.Cut(raw, "\n\n")
	if !ok {
		return nil, fmt.Errorf("breakglass: %s: malformed commit", shortID(commit))
	}
	var tree string
	var parents []string
	for _, line := range strings.Split(hdr, "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "tree":
			tree = v
		case "parent":
			parents = append(parents, v)
		}
	}
	if len(parents) != 1 {
		return nil, fmt.Errorf("breakglass: %s: a break-glass commit has exactly one parent", shortID(commit))
	}
	i := strings.LastIndex(msg, trailerSignature+": ")
	if i < 0 {
		return nil, fmt.Errorf("breakglass: %s is not a break-glass commit (no signature)", shortID(commit))
	}
	signed, sigLine := msg[:i], strings.TrimSpace(msg[i+len(trailerSignature)+2:])
	sig, err := base64.StdEncoding.DecodeString(sigLine)
	if err != nil {
		return nil, fmt.Errorf("breakglass: %s: bad signature encoding", shortID(commit))
	}
	s := &Signed{Commit: commit, Session: trailer(signed, trailerSession), Actor: trailer(signed, trailerActor), Key: trailer(signed, trailerKey)}
	for _, k := range keys {
		if KeyFingerprint(k) == s.Key && ed25519.Verify(k, signedPayload(tree, parents[0], signed), sig) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("breakglass: %s: signature does not verify against a trusted key (key %s)", shortID(commit), s.Key)
}

// BreakGlassCommits は from..to に入った break-glass コミット（署名 trailer を持つもの）を返す。
func BreakGlassCommits(ctx context.Context, repo, from, to string) []string {
	out, err := runGit(ctx, repo, "log", "--format=%H", "--grep=^"+trailerSignature+": ", from+".."+to)
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func trailer(msg, key string) string {
	for _, line := range strings.Split(msg, "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ── ミラー側: 保持・事後合流・session の終わり ──────────────────────────────────

// holdBreakGlass は session を開いているミラーの Sync。中央を追わない（緊急書込を
// 巻き戻さない）。中央に届けば合流用ブランチを push して PR の下書きを作り、中央の
// 先端が break-glass の先端を祖先に含めば session を閉じる（closed=true）。
func (m *Mirror) holdBreakGlass(ctx context.Context, sess *Session) (closed bool, err error) {
	logf := m.logf
	tip, err := runGit(ctx, m.Dir, "rev-parse", "--verify", "-q", "refs/heads/"+sess.Branch)
	if err != nil {
		return false, fmt.Errorf("breakglass: mirror has no branch %s", sess.Branch)
	}
	ref := upstreamRef + sess.Branch
	// 名前付き remote ではなく URL で取る（mirror の refspec で refs/heads が上書きされない）。
	if _, err := runGit(ctx, m.Dir, "fetch", "--quiet", sess.Upstream, "+refs/heads/"+sess.Branch+":"+ref); err != nil {
		logf("holding break-glass session %s: central git still unreachable; serving the break-glass head %s", sess.ID, shortID(tip))
		return false, fmt.Errorf("breakglass session %s: central git unreachable: %w", sess.ID, err)
	}
	central, err := runGit(ctx, m.Dir, "rev-parse", ref)
	if err != nil {
		return false, err
	}
	lg := audit.NewLog(sess.AuditLog)
	if tip == sess.Base {
		// 書けなかった session（push 前に止まった）は合流するものが無い。
		if err := lg.Append(audit.Record{Kind: audit.KindBreakGlassMerged, Session: sess.ID,
			Detail: "no break-glass commit reached the mirror; nothing to merge back"}); err != nil {
			return false, err
		}
		return true, m.closeSession(ctx, sess)
	}
	if _, err := runGit(ctx, m.Dir, "merge-base", "--is-ancestor", tip, central); err == nil {
		// 中央が取り込んだ: session を閉じ、使った鍵を失効させる。
		if err := lg.Append(audit.Record{Kind: audit.KindBreakGlassMerged, Session: sess.ID, Commit: tip,
			Detail: fmt.Sprintf("central %s %s contains the break-glass head; the mirror follows the central again", sess.Branch, shortID(central))}); err != nil {
			return false, err
		}
		for _, k := range sess.Keys {
			if err := lg.Append(audit.Record{Kind: audit.KindBreakGlassKeyRetired, Session: sess.ID, Key: k,
				Detail: "used in a merged break-glass session; rotate it"}); err != nil {
				return false, err
			}
		}
		logf("break-glass session %s merged into the central %s (%s); key(s) %v retired — rotate them", sess.ID, sess.Branch, shortID(central), sess.Keys)
		return true, m.closeSession(ctx, sess)
	}
	if sess.MergeTip != tip {
		branch := MergeBranchPrefix + sess.ID
		if _, err := runGit(ctx, m.Dir, "push", "--quiet", sess.Upstream, "+"+tip+":refs/heads/"+branch); err != nil {
			return false, fmt.Errorf("breakglass session %s: push the merge-back branch: %w", sess.ID, err)
		}
		draft, err := m.writePRDraft(ctx, sess, tip, central, branch)
		if err != nil {
			return false, err
		}
		if err := lg.Append(audit.Record{Kind: audit.KindBreakGlassMergeBack, Session: sess.ID, Commit: tip,
			Detail: fmt.Sprintf("pushed %s to %s; PR draft %s", branch, sess.Upstream, draft)}); err != nil {
			return false, err
		}
		sess.MergeBranch, sess.MergeTip, sess.PRDraft = branch, tip, draft
		if err := writeSession(m.Dir, sess); err != nil {
			return false, err
		}
		logf("break-glass session %s: merge-back branch %s pushed to the central; PR draft at %s", sess.ID, branch, draft)
	}
	logf("holding break-glass session %s: waiting for the central %s to contain %s (merge %s with a merge commit, not squash)",
		sess.ID, sess.Branch, shortID(tip), sess.MergeBranch)
	return false, nil
}

func (m *Mirror) closeSession(ctx context.Context, sess *Session) error {
	_, _ = runGit(ctx, m.Dir, "update-ref", "-d", upstreamRef+sess.Branch)
	if err := os.Remove(filepath.Join(m.Dir, SessionFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("breakglass: %w", err)
	}
	return nil
}

// writePRDraft は事後合流 PR の下書き（Markdown）をミラーの横に書く。競合は自動で
// 解かない: 中央が base から進んでいれば、両側で変わったパスを並べて人の解消に渡す。
func (m *Mirror) writePRDraft(ctx context.Context, sess *Session, tip, central, branch string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# [break-glass] 事後合流: %s\n\n", sess.ID)
	fmt.Fprintf(&b, "中央の git に届かない間にローカルミラーへ書いた break-glass コミットを、中央の `%s` へ合流する PR です（§25.1 (b)）。\n\n", sess.Branch)
	fmt.Fprintf(&b, "- head: `%s`（%s）\n- base: `%s`（session を開いたときの中央の先端）\n- 中央の `%s`: `%s`\n- 開始: %s\n- S4 監査: `%s`\n\n",
		branch, shortID(tip), shortID(sess.Base), sess.Branch, shortID(central), sess.Opened.Format(time.RFC3339), sess.AuditLog)
	b.WriteString("## break-glass コミット\n\n| コミット | 実行者 | 鍵 | 件名 |\n|---|---|---|---|\n")
	if out, err := runGit(ctx, m.Dir, "log", "--reverse", "--format=%H%x1f%s%x1f%(trailers:key="+trailerActor+",valueonly,separator=)%x1f%(trailers:key="+trailerKey+",valueonly,separator=)", sess.Base+".."+tip); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, "\x1f")
			for len(f) < 4 {
				f = append(f, "")
			}
			fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s |\n", shortID(f[0]), f[2], f[3], f[1])
		}
	}
	b.WriteString("\n## 競合\n\n")
	if central == sess.Base {
		b.WriteString("中央は base から進んでいません（fast-forward で取り込めます）。\n")
	} else {
		n, _ := runGit(ctx, m.Dir, "rev-list", "--count", sess.Base+".."+central)
		ours := pathSet(ctx, m.Dir, sess.Base, tip)
		var both []string
		for _, p := range sortedKeysOf(pathSet(ctx, m.Dir, sess.Base, central)) {
			if ours[p] {
				both = append(both, p)
			}
		}
		fmt.Fprintf(&b, "中央は base から %s コミット進んでいます。\n\n", n)
		if len(both) == 0 {
			b.WriteString("両側で変わったパスはありません。\n")
		} else {
			b.WriteString("**両側で変わったパス（競合の可能性）:**\n\n")
			for _, p := range both {
				fmt.Fprintf(&b, "- `%s`\n", p)
			}
			b.WriteString("\n解消は人が行う: 中央を正とし、合流ブランチに中央の先端を merge して break-glass 側の変更を載せ直す（rebase・squash はしない）。\n")
		}
	}
	b.WriteString("\n## 取り込み方\n\n")
	b.WriteString("- **merge commit（または fast-forward）で取り込む。** squash・rebase は署名付きの break-glass コミットを正の履歴から消すので使わない。\n")
	b.WriteString("- 中央の先端がこのブランチの先端を含むまで、ミラーは中央を追わずに break-glass の先端を配り続ける（緊急書込を古い中央で巻き戻さない）。\n")
	b.WriteString("- 取り込まれるとミラーは session を閉じ、使った break-glass 鍵を失効させる（S4 に記録）。鍵は rotate する。\n")
	b.WriteString("- 署名の確認: `sakutei breakglass verify --repo <clone> --pubkey <鍵.pub> --range " + shortID(sess.Base) + ".." + shortID(tip) + "`\n")
	path := filepath.Join(m.Dir, "sakutei-breakglass-"+sess.ID+"-pr.md")
	if err := writeFileAtomic(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func pathSet(ctx context.Context, repo, from, to string) map[string]bool {
	out, err := runGit(ctx, repo, "diff", "--name-only", from, to)
	set := map[string]bool{}
	if err != nil || out == "" {
		return set
	}
	for _, p := range strings.Split(out, "\n") {
		set[p] = true
	}
	return set
}

// ── 小物 ──────────────────────────────────────────────────────────────────────

func keyRetired(auditLog, fp string) (bool, error) {
	recs, err := audit.ReadAll(auditLog)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.Kind == audit.KindBreakGlassKeyRetired && r.Key == fp {
			return true, nil
		}
	}
	return false, nil
}

// localPath はローカルミラーの URL（file:// またはパス）をディレクトリにする。
func localPath(url string) (string, error) {
	if p, ok := strings.CutPrefix(url, "file://"); ok {
		return p, nil
	}
	if strings.Contains(url, "://") || (strings.Contains(url, ":") && !filepath.IsAbs(url)) {
		return "", fmt.Errorf("breakglass: origin %s is not a local path; a break-glass write goes to the site's local mirror (§25.2)", url)
	}
	return absPath(url), nil
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func shortID(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("breakglass: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("breakglass: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("breakglass: %w", err)
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return fmt.Errorf("breakglass: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("breakglass: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("breakglass: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("breakglass: %w", err)
	}
	return nil
}
