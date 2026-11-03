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

//go:build integration_v3_s3

package integration

// lab_test.go — サーバを立てて消す・手で現実を見る・変える・v3 の CLI を回す。
//
// サーバ（イメージは環境変数で差し替えられる。既定は v2 の統合テストと同じ tag と
// digest）：
//
//	seaweedfs      SAKUTEI_IT_SEAWEEDFS_IMAGE      必須
//	minio          SAKUTEI_IT_MINIO_IMAGE          任意
//	seaweedfs-old  SAKUTEI_IT_SEAWEEDFS_OLD_IMAGE  任意
//
// 必須のサーバはイメージが取れなければ失敗にする。任意のサーバはイメージが取れ
// ない（docker pull が失敗する）か環境変数が空なら、そのサーバの段だけを飛ばし、
// 理由をログに出す。seaweedfs-old（バージョニング・ポリシー・タグの PUT が
// NotImplemented の版）は、片方にしか無い API の扱いを本物で見るため。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/s3"
)

// 既定のイメージ。v2 の統合テストと同じ digest（2026-10-27 に docker pull で確かめた値）。
const (
	seaweedfsImage    = "chrislusf/seaweedfs:4.51@sha256:f7a49e3f3972ebed5be2ed4befa4a0c68eb7bdddba6e692d42729ceb545134b8"
	minioImage        = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"
	seaweedfsOldImage = "chrislusf/seaweedfs:3.80@sha256:1055999e08eed1789b0ae45d235126e4495e23d3fb9d6396293fd42539b1ae6a"
)

const (
	prefix    = "sakutei-v3s3-"
	accessKey = "sakutei-it"
	secretKey = "sakutei-it-secret-key-0123456789"
)

type server struct {
	name, image, container, url string
	port                        string   // コンテナの中のポート
	args                        []string // コンテナに渡す引数
	runOpts                     []string // docker run の足す引数（-e・-v）
	optional                    bool
	warmup                      bool // 最初のオブジェクトの PUT を済ませてから始める（SeaweedFS）
}

var lab struct {
	dir     string
	servers map[string]*server
	skipped map[string]string // 任意で使わなかったサーバ → 理由
	clients map[string]*s3.Client
	secret  string // 管理者の secret key のファイル
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-v3-s3: %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	return m.Run()
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{"-n", "docker"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func image(env, def string) string {
	if v, ok := os.LookupEnv(env); ok {
		return v
	}
	return def
}

func setup() error {
	if out, err := docker("version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker（sudo -n で）が要る: %v: %s", err, out)
	}
	dir, err := os.MkdirTemp("", "sakutei-v3s3-it-")
	if err != nil {
		return err
	}
	lab.dir = dir
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	// SeaweedFS の資格（コンテナの利用者が読めるように 0644）。
	weedIAM := fmt.Sprintf(`{"identities":[{"name":"sakutei","credentials":[{"accessKey":%q,"secretKey":%q}],"actions":["Admin","Read","List","Tagging","Write"]}]}`, accessKey, secretKey)
	iam := filepath.Join(dir, "weed-s3.json")
	if err := os.WriteFile(iam, []byte(weedIAM), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(iam, 0o644); err != nil { // umask に関わらず。
		return err
	}
	lab.secret = filepath.Join(dir, "admin.secret")
	if err := os.WriteFile(lab.secret, []byte(secretKey+"\n"), 0o600); err != nil {
		return err
	}
	// -volume.max の既定（8）ではバケットごとにボリュームを約 7 個使うので、3 つ目の
	// バケットからオブジェクトの PUT が 500 になる。小さいボリュームを多く持てるようにする。
	weedArgs := []string{"server", "-s3", "-s3.config=/etc/sakutei/weed-s3.json", "-dir=/data",
		"-volume.max=1000", "-master.volumeSizeLimitMB=64"}
	weedMount := []string{"-v", iam + ":/etc/sakutei/weed-s3.json:ro"}
	all := []*server{
		{name: "seaweedfs", image: image("SAKUTEI_IT_SEAWEEDFS_IMAGE", seaweedfsImage), port: "8333", args: weedArgs, runOpts: weedMount, warmup: true},
		{name: "minio", image: image("SAKUTEI_IT_MINIO_IMAGE", minioImage), port: "9000", optional: true,
			args: []string{"server", "/data"}, runOpts: []string{"-e", "MINIO_ROOT_USER=" + accessKey, "-e", "MINIO_ROOT_PASSWORD=" + secretKey}},
		{name: "seaweedfs-old", image: image("SAKUTEI_IT_SEAWEEDFS_OLD_IMAGE", seaweedfsOldImage), port: "8333", args: weedArgs, runOpts: weedMount, optional: true, warmup: true},
	}
	lab.servers = map[string]*server{}
	lab.skipped = map[string]string{}
	for _, s := range all {
		if s.image == "" {
			if !s.optional {
				return fmt.Errorf("%s: イメージが空", s.name)
			}
			lab.skipped[s.name] = "イメージが空"
			fmt.Fprintf(os.Stderr, "integration-v3-s3: %s（任意）は使わない: %s\n", s.name, lab.skipped[s.name])
			continue
		}
		if out, err := docker("image", "inspect", s.image); err != nil {
			if out, err = docker("pull", s.image); err != nil {
				if !s.optional {
					return fmt.Errorf("%s: docker pull %s: %v: %s", s.name, s.image, err, lastLine(out))
				}
				lab.skipped[s.name] = fmt.Sprintf("docker pull %s に失敗: %s", s.image, lastLine(out))
				fmt.Fprintf(os.Stderr, "integration-v3-s3: %s（任意）は使わない: %s\n", s.name, lab.skipped[s.name])
				continue
			}
		}
		s.container = prefix + s.name
		_, _ = docker("rm", "-f", "-v", s.container) // 前の回の残り。
		args := append([]string{"run", "-d", "--name", s.container, "-p", "127.0.0.1::" + s.port}, s.runOpts...)
		args = append(append(args, s.image), s.args...)
		if out, err := docker(args...); err != nil {
			return fmt.Errorf("%s: docker run: %v: %s", s.name, err, out)
		}
		lab.servers[s.name] = s
		hp, err := docker("port", s.container, s.port+"/tcp")
		if err != nil {
			return fmt.Errorf("%s: docker port: %v: %s", s.name, err, hp)
		}
		s.url = "http://" + strings.Split(hp, "\n")[0]
	}
	// 手で現実を見る・変える Client（sakutei の外の操作。管理者の資格）。
	lab.clients = map[string]*s3.Client{}
	for name, s := range lab.servers {
		c, err := s3.NewClient(s3.ClientConfig{Endpoint: s.url, Credentials: creds(accessKey, secretKey), Timeout: 5 * time.Second})
		if err != nil {
			return err
		}
		lab.clients[name] = c
	}
	// 立ち上がるのを待つ（ListBuckets が通るまで）。SeaweedFS は起動して最初の
	// オブジェクトの PUT に 15 秒ほどかかることがあるので、それも済ませておく。
	for name, s := range lab.servers {
		err := waitReady(name, 3*time.Minute)
		if err == nil && s.warmup {
			err = warmup(name, 2*time.Minute)
		}
		if err != nil {
			out, _ := docker("logs", "--tail", "20", s.container)
			return fmt.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	return nil
}

func creds(ak, sk string) func(context.Context) (s3.Credentials, error) {
	return func(context.Context) (s3.Credentials, error) {
		return s3.Credentials{AccessKey: ak, SecretKey: sk}, nil
	}
}

func waitReady(name string, d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := lab.clients[name].Open(ctx)
		if err == nil {
			_, _, err = s.ListBuckets(ctx)
		}
		cancel()
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s たっても立ち上がらない: %v", d, last)
}

// warmup は扱う接頭辞の外のバケットにオブジェクトを 1 つ PUT し（通るまで繰り返す）、消す。
func warmup(name string, d time.Duration) error {
	const bucket = "sakutei-warmup"
	ctx := context.Background()
	sess, err := lab.clients[name].Open(ctx)
	if err != nil {
		return err
	}
	if err := sess.CreateBucket(ctx, bucket, false); err != nil {
		return fmt.Errorf("warmup: %v", err)
	}
	url := lab.servers[name].url + "/" + bucket + "/warmup.txt"
	client := &http.Client{Timeout: d}
	deadline := time.Now().Add(d)
	for {
		code, err := objectRequest(client, http.MethodPut, url, []byte("warmup\n"))
		if err == nil && code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("warmup: 最初のオブジェクトの PUT が %s のうちに通らない: HTTP %d, %v", d, code, err)
		}
		time.Sleep(time.Second)
	}
	if code, err := objectRequest(client, http.MethodDelete, url, nil); err != nil || code/100 != 2 {
		return fmt.Errorf("warmup: オブジェクトの DELETE: HTTP %d, %v", code, err)
	}
	return sess.DeleteBucket(ctx, bucket)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// need はそのサーバを使っていなければ t を飛ばす。
func need(t *testing.T, name string) {
	t.Helper()
	if _, ok := lab.servers[name]; !ok {
		t.Skipf("%s は使っていない: %s", name, lab.skipped[name])
	}
}

func teardown() {
	var names []string
	for _, s := range lab.servers {
		_, _ = docker("unpause", s.container)
		names = append(names, s.container)
	}
	if len(names) > 0 {
		if out, err := docker(append([]string{"rm", "-f", "-v"}, names...)...); err != nil {
			fmt.Fprintf(os.Stderr, "integration-v3-s3: docker rm %v: %v: %s\n", names, err, out)
		}
	}
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
}

// mainServers は 4 つの API を持つサーバ（必須の seaweedfs と、使えれば minio）。
func mainServers() []string {
	var out []string
	for _, n := range []string{"seaweedfs", "minio"} {
		if _, ok := lab.servers[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 現実を手で見る・変える
// ─────────────────────────────────────────────────────────────────────────────

func session(t *testing.T, name string) *s3.Session {
	t.Helper()
	s, err := lab.clients[name].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func hasBucket(t *testing.T, name, bucket string) bool {
	t.Helper()
	bs, _, err := session(t, name).ListBuckets(context.Background())
	if err != nil {
		t.Fatalf("%s: ListBuckets: %v", name, err)
	}
	for _, b := range bs {
		if b.Name == bucket {
			return true
		}
	}
	return false
}

// putObject はバケットにオブジェクトを 1 つ入れる（provider はオブジェクトを扱わない
// ので、ここで SigV4 の PUT を直に送る）。
func putObject(t *testing.T, name, bucket, key string) {
	t.Helper()
	code, err := objectRequest(&http.Client{Timeout: time.Minute}, http.MethodPut, lab.servers[name].url+"/"+bucket+"/"+key, []byte("sakutei integration test object\n"))
	if err != nil || code != http.StatusOK {
		t.Fatalf("%s: PUT %s/%s: HTTP %d, %v", name, bucket, key, code, err)
	}
}

func deleteObject(t *testing.T, name, bucket, key string) {
	t.Helper()
	code, err := objectRequest(&http.Client{Timeout: time.Minute}, http.MethodDelete, lab.servers[name].url+"/"+bucket+"/"+key, nil)
	if err != nil || code/100 != 2 {
		t.Fatalf("%s: DELETE %s/%s: HTTP %d, %v", name, bucket, key, code, err)
	}
}

// objectRequest はオブジェクトへの SigV4 の要求を 1 つ送り、状態コードを返す。
func objectRequest(c *http.Client, method, url string, body []byte) (int, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	req.Header.Set("X-Amz-Content-Sha256", sum)
	s3.SignV4(req, sum, s3.Credentials{AccessKey: accessKey, SecretKey: secretKey}, s3.DefaultRegion, "s3", time.Now())
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// intent・つなぎ方の設定・CLI
// ─────────────────────────────────────────────────────────────────────────────

// endpointCfg は settings.endpoints の 1 つ。
type endpointCfg struct {
	name, server string
	fields       []string
	aliases      map[string]string
	// observe・write は {access_key, secret_key_file}（空なら管理者）。
	observe, write [2]string
}

// ws は 1 つの段の作業の場所（intent・設定・Record）。
type ws struct {
	dir string
}

func newWS(t *testing.T, name string, eps ...endpointCfg) ws {
	t.Helper()
	w := ws{dir: filepath.Join(lab.dir, name)}
	if err := os.MkdirAll(filepath.Join(w.dir, "intent"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("providers:\n  s3:\n    module: s3\n    settings:\n      endpoints:\n")
	for _, e := range eps {
		fmt.Fprintf(&b, "        %s:\n          endpoint: %s\n          timeout: 5s\n", e.name, lab.servers[e.server].url)
		if e.observe[0] == "" {
			e.observe = [2]string{accessKey, lab.secret}
		}
		fmt.Fprintf(&b, "          observe: {access_key: %s, secret_key_file: %s}\n", e.observe[0], e.observe[1])
		if e.write[0] != "" {
			fmt.Fprintf(&b, "          write: {access_key: %s, secret_key_file: %s}\n", e.write[0], e.write[1])
		}
		if e.fields != nil {
			fmt.Fprintf(&b, "          fields: [%s]\n", strings.Join(e.fields, ", "))
		}
		if len(e.aliases) > 0 {
			var kv []string
			for k, v := range e.aliases {
				kv = append(kv, k+": "+v)
			}
			sort.Strings(kv)
			fmt.Fprintf(&b, "          storage_class_aliases: {%s}\n", strings.Join(kv, ", "))
		}
	}
	if err := os.WriteFile(filepath.Join(w.dir, "config.yaml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return w
}

type rangeDecl struct {
	Provider string            `json:"provider"`
	Args     map[string]string `json:"args"`
	Prune    bool              `json:"prune,omitempty"`
}

type resource struct {
	Type   string         `json:"type"`
	ID     string         `json:"id"`
	Ensure string         `json:"ensure,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

func bucketRes(id string, fields map[string]any) resource {
	return resource{Type: "s3/bucket", ID: id, Fields: fields}
}

func rng(endpoint, pfx string, prune bool) rangeDecl {
	return rangeDecl{Provider: "s3", Args: map[string]string{"endpoint": endpoint, "prefix": pfx}, Prune: prune}
}

// intent は intent/intent.json を書き換える。
func (w ws) intent(t *testing.T, ranges []rangeDecl, rs ...resource) {
	t.Helper()
	b, err := json.MarshalIndent(map[string]any{"ranges": ranges, "resources": rs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "intent", "intent.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// run は v3 の CLI を回す（plan・reconcile は --json の結果を読む）。
func (w ws) run(t *testing.T, verb string) (plan.Result, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{verb, "--intent", filepath.Join(w.dir, "intent"),
		"--config", filepath.Join(w.dir, "config.yaml"), "--record", filepath.Join(w.dir, "record"), "--json"},
		&stdout, &stderr, cli.WithModule("s3", s3.Module))
	var r plan.Result
	if code == cli.ExitRejected {
		return r, code, stderr.String()
	}
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("%s の出力を読めない（exit %d）: %v\n%s\n%s", verb, code, err, stdout.String(), stderr.String())
	}
	return r, code, stderr.String()
}

// approve は承認の依頼を alice で承認する。
func (w ws) approve(t *testing.T, id string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"approve", id, "--record", filepath.Join(w.dir, "record"), "--as", "alice"},
		&stdout, &stderr, cli.WithModule("s3", s3.Module)); code != cli.ExitOK {
		t.Fatalf("approve %s exit %d:\n%s%s", id, code, stdout.String(), stderr.String())
	}
}

func changeOf(r plan.Result, resource string) plan.Change {
	for _, c := range r.Changes {
		if c.Resource == resource {
			return c
		}
	}
	return plan.Change{}
}

func dump(r plan.Result) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

// wantOps は resource の op を確かめる。
func wantOps(t *testing.T, step string, r plan.Result, want map[string]plan.Op) {
	t.Helper()
	for res, op := range want {
		if c := changeOf(r, res); c.Op != op {
			t.Fatalf("%s: %s は %q（want %q）\n%s", step, res, c.Op, op, dump(r))
		}
	}
}

// converged は全部の宣言が宣言どおり（none か、当てて読み直すと none）か。
func converged(r plan.Result) bool {
	for _, c := range r.Changes {
		switch {
		case c.Op == plan.None || c.Op == plan.Unmanaged:
		case c.Outcome == plan.OutcomeOK && c.After == plan.None:
		default:
			return false
		}
	}
	return true
}
