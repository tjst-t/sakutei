//go:build integration_s3

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

package s3

// サーバ（イメージは環境変数で差し替えられる。既定は tag と digest で固定）:
//
//	seaweedfs      SAKUTEI_IT_SEAWEEDFS_IMAGE      必須
//	minio          SAKUTEI_IT_MINIO_IMAGE          任意
//	seaweedfs-old  SAKUTEI_IT_SEAWEEDFS_OLD_IMAGE  任意
//
// 必須のサーバはイメージが取れなければ失敗にする。任意のサーバはイメージが
// 取れない（docker pull が失敗する）か環境変数が空なら、そのサーバの段だけを
// 飛ばし、理由をログに出す。seaweedfs-old（バージョニング・ポリシー・タグの
// PUT が NotImplemented の版）は、片方にしか無い API の扱いを本物で見るため。

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
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/s3bucket"
)

// 既定のイメージ。digest は docker pull で確かめた値（2026-10-27）。
const (
	seaweedfsImage    = "chrislusf/seaweedfs:4.51@sha256:f7a49e3f3972ebed5be2ed4befa4a0c68eb7bdddba6e692d42729ceb545134b8"
	minioImage        = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"
	seaweedfsOldImage = "chrislusf/seaweedfs:3.80@sha256:1055999e08eed1789b0ae45d235126e4495e23d3fb9d6396293fd42539b1ae6a"
)

const (
	accessKey = "sakutei-it"
	secretKey = "sakutei-it-secret-key-0123456789"
	prefix    = "sakutei-s3it-"
)

type server struct {
	name, image, container, url string
	port                        string   // コンテナの中のポート
	args                        []string // コンテナに渡す引数
	runOpts                     []string // docker run の追加の引数（-e・-v）
	optional                    bool
	warmup                      bool // 最初のオブジェクトの PUT を済ませてから始める（SeaweedFS）
}

var lab struct {
	dir     string
	bin     string
	s3cfg   string
	servers map[string]*server
	skipped map[string]string // 任意で使わなかったサーバ → 理由
	clients map[string]*s3bucket.Client
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-s3: %v\n", err)
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
		return fmt.Errorf("docker (through sudo -n) is required: %v: %s", err, out)
	}
	dir, err := os.MkdirTemp("", "sakutei-s3-it-")
	if err != nil {
		return err
	}
	lab.dir = dir
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	// SeaweedFS の資格（コンテナの利用者が読めるように 0644）。
	weedIAM := fmt.Sprintf(`{"identities":[{"name":"sakutei","credentials":[{"accessKey":%q,"secretKey":%q}],"actions":["Admin","Read","List","Tagging","Write"]}]}`, accessKey, secretKey)
	if err := os.WriteFile(filepath.Join(dir, "weed-s3.json"), []byte(weedIAM), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "weed-s3.json"), 0o644); err != nil { // umask に関わらず。
		return err
	}
	// -volume.max の既定（8）ではバケットごとにボリュームを約 7 個使うので、
	// 3 つ目のバケットからオブジェクトの PUT が 500 になる。小さいボリュームを
	// 多く持てるようにする。
	weedArgs := []string{"server", "-s3", "-s3.config=/etc/sakutei/weed-s3.json", "-dir=/data",
		"-volume.max=1000", "-master.volumeSizeLimitMB=64"}
	weedMount := []string{"-v", dir + "/weed-s3.json:/etc/sakutei/weed-s3.json:ro"}
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
				return fmt.Errorf("%s: the image must not be empty", s.name)
			}
			lab.skipped[s.name] = "the image is empty"
			fmt.Fprintf(os.Stderr, "integration-s3: %s (optional) is not used: %s\n", s.name, lab.skipped[s.name])
			continue
		}
		if out, err := docker("pull", s.image); err != nil {
			if !s.optional {
				return fmt.Errorf("%s: docker pull %s: %v: %s", s.name, s.image, err, lastLine(out))
			}
			lab.skipped[s.name] = fmt.Sprintf("docker pull %s failed: %s", s.image, lastLine(out))
			fmt.Fprintf(os.Stderr, "integration-s3: %s (optional) is not used: %s\n", s.name, lab.skipped[s.name])
			continue
		}
		s.container = prefix + s.name
		_, _ = docker("rm", "-f", "-v", s.container) // 前の回の残り。
		args := append([]string{"run", "-d", "--name", s.container, "-p", "127.0.0.1::" + s.port}, s.runOpts...)
		args = append(append(args, s.image), s.args...)
		if out, err := docker(args...); err != nil {
			return fmt.Errorf("%s: docker run: %v: %s", s.name, err, out)
		}
		hp, err := docker("port", s.container, s.port+"/tcp")
		if err != nil {
			return fmt.Errorf("%s: docker port: %v: %s", s.name, err, hp)
		}
		s.url = "http://" + strings.Split(hp, "\n")[0]
		lab.servers[s.name] = s
	}

	// secret store と endpoint の一覧。
	store := map[string]any{"secrets": map[string]any{
		"s3/access_key": map[string]any{"gen": 1, "value": accessKey},
		"s3/secret_key": map[string]any{"gen": 1, "value": secretKey},
	}}
	b, _ := json.Marshal(store)
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), b, 0o600); err != nil {
		return err
	}
	var cfg strings.Builder
	cfg.WriteString("secret_store: secrets.json\nendpoints:\n")
	for _, name := range serverNames() {
		fmt.Fprintf(&cfg, "  %s:\n    endpoint: %s\n    access_key: secretref:file/s3/access_key\n    secret_key: secretref:file/s3/secret_key\n    bucket_prefix: sk-it-\n    timeout: 5s\n", name, lab.servers[name].url)
	}
	// S3 互換実装との差異への対応（#132）: 同じサーバを別の名前・別の
	// bucket_prefix（sk-it- と重ならない）で、扱う設定を絞って足す。
	fmt.Fprintf(&cfg, "  seaweedfs-compat:\n    endpoint: %s\n    access_key: secretref:file/s3/access_key\n    secret_key: secretref:file/s3/secret_key\n    bucket_prefix: sk-compat-\n    timeout: 5s\n    fields:\n      - policy\n      - tags\n    storage_class_aliases:\n      VENDOR_COLD: GLACIER\n", lab.servers["seaweedfs"].url)
	lab.s3cfg = filepath.Join(dir, "s3.yaml")
	if err := os.WriteFile(lab.s3cfg, []byte(cfg.String()), 0o600); err != nil {
		return err
	}

	// 手で現実を変えるための Client（sakutei の外の操作）。
	lab.clients = map[string]*s3bucket.Client{}
	for name, s := range lab.servers {
		c, err := s3bucket.NewClient(s3bucket.ClientConfig{Endpoint: s.url, Credentials: creds, Timeout: 5 * time.Second})
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

	lab.bin = filepath.Join(dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = "../../.."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	return nil
}

func creds(context.Context) (s3bucket.Credentials, error) {
	return s3bucket.Credentials{AccessKey: accessKey, SecretKey: secretKey}, nil
}

func waitReady(name string, d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := lab.clients[name].Open(ctx)
		if err == nil {
			_, err = s.ListBuckets(ctx)
		}
		cancel()
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("not ready after %s: %v", d, last)
}

// warmup は bucket_prefix の外のバケットにオブジェクトを 1 つ PUT し（通るまで
// 繰り返す）、消す。
func warmup(name string, d time.Duration) error {
	const bucket = "sakutei-warmup"
	ctx := context.Background()
	sess, err := lab.clients[name].Open(ctx)
	if err != nil {
		return err
	}
	if err := sess.CreateBucket(ctx, bucket); err != nil {
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
			return fmt.Errorf("warmup: the first object PUT did not succeed within %s: HTTP %d, %v", d, code, err)
		}
		time.Sleep(time.Second)
	}
	if code, err := objectRequest(client, http.MethodDelete, url, nil); err != nil || code/100 != 2 {
		return fmt.Errorf("warmup: DELETE object: HTTP %d, %v", code, err)
	}
	return sess.DeleteBucket(ctx, bucket)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// need はそのサーバが使われていなければ t を飛ばす。
func need(t *testing.T, name string) {
	t.Helper()
	if _, ok := lab.servers[name]; !ok {
		t.Skipf("%s is not used: %s", name, lab.skipped[name])
	}
}

func teardown() {
	for _, s := range lab.servers {
		_, _ = docker("unpause", s.container)
		if out, err := docker("rm", "-f", "-v", s.container); err != nil {
			fmt.Fprintf(os.Stderr, "integration-s3: docker rm %s: %v: %s\n", s.container, err, out)
		}
	}
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
}

func serverNames() []string {
	var out []string
	for _, n := range []string{"seaweedfs", "minio", "seaweedfs-old"} {
		if _, ok := lab.servers[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// ---- 現実を手で見る・変える ----

func session(t *testing.T, name string) *s3bucket.Session {
	t.Helper()
	s, err := lab.clients[name].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func hasBucket(t *testing.T, name, bucket string) bool {
	t.Helper()
	bs, err := session(t, name).ListBuckets(context.Background())
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

// putObject はバケットにオブジェクトを 1 つ入れる（provider はオブジェクトを
// 扱わないので、ここで SigV4 の PUT を直に送る）。
func putObject(t *testing.T, name, bucket, key string) {
	t.Helper()
	code, err := objectRequest(&http.Client{Timeout: time.Minute}, http.MethodPut, lab.servers[name].url+"/"+bucket+"/"+key, []byte("sakutei integration test object\n"))
	if err != nil {
		t.Fatalf("%s: PUT %s/%s: %v", name, bucket, key, err)
	}
	if code != http.StatusOK {
		t.Fatalf("%s: PUT %s/%s: HTTP %d", name, bucket, key, code)
	}
}

// objectRequest はオブジェクトへの SigV4 の要求を 1 つ送り、状態コードを返す。
func objectRequest(c *http.Client, method, url string, body []byte) (int, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	sum := fmt.Sprintf("%x", sha256sum(body))
	req.Header.Set("X-Amz-Content-Sha256", sum)
	s3bucket.SignV4(req, sum, s3bucket.Credentials{AccessKey: accessKey, SecretKey: secretKey}, s3bucket.DefaultRegion, "s3", time.Now())
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// ---- intent と CLI ----

type resource struct {
	Provider       string            `json:"provider"`
	ID             string            `json:"id"`
	Fields         map[string]string `json:"fields"`
	DeletionPolicy string            `json:"deletion_policy,omitempty"`
}

func policyFor(bucket string) string {
	return `{"Version": "2012-10-17", "Statement": [{"Sid": "public-read", "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::` + bucket + `/*"}]}`
}

const lifecycle = `[{"ID": "expire-logs", "Status": "Enabled", "Filter": {"Prefix": "logs/"}, "Expiration": {"Days": 30}},
 {"ID": "noncurrent", "Status": "Enabled", "Filter": {}, "NoncurrentVersionExpiration": {"NoncurrentDays": 7}}]`

// appBucket は全部のフィールドを宣言したバケット。
func appBucket(ep string, prune bool) resource {
	r := resource{Provider: s3bucket.Name, ID: ep + "/sk-it-app", Fields: map[string]string{
		"versioning": "Enabled",
		"policy":     policyFor("sk-it-app"),
		"lifecycle":  lifecycle,
		"tags":       `{"env": "integration", "owner": "sakutei"}`,
	}}
	if prune {
		r.DeletionPolicy = "PruneWithinScope"
	}
	return r
}

// appBuckets は 4 つの API を持つサーバ（main）ごとの appBucket。
func appBuckets(main []string, prune bool, more ...resource) []resource {
	var rs []resource
	for _, ep := range main {
		rs = append(rs, appBucket(ep, prune))
	}
	return append(rs, more...)
}

func writeIntent(t *testing.T, rs ...resource) {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "buckets.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliArgs(verb string, extra ...string) []string {
	return append([]string{verb,
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--cause-state", filepath.Join(lab.dir, "cause-state.json"),
		"--s3-config", lab.s3cfg,
	}, extra...)
}

func sakutei(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(lab.bin, cliArgs(verb, extra...)...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out), 0
}

func mustReconcile(t *testing.T) string {
	t.Helper()
	out, code := sakutei(t, "reconcile")
	if code != 0 {
		t.Fatalf("reconcile exited %d:\n%s", code, out)
	}
	return out
}

type statusDoc struct {
	Complete bool `json:"complete"`
	Summary  struct {
		Conformant   int `json:"conformant"`
		Drift        int `json:"drift"`
		Unobservable int `json:"unobservable"`
	} `json:"summary"`
	Resources []struct {
		LocalID string `json:"local_id"`
		State   string `json:"state"`
	} `json:"resources"`
}

func status(t *testing.T) (statusDoc, string) {
	t.Helper()
	cmd := exec.Command(lab.bin, cliArgs("status", "--json")...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		t.Fatalf("sakutei status: %v", err)
	}
	var doc statusDoc
	if jerr := json.Unmarshal(out, &doc); jerr != nil {
		t.Fatalf("status --json: %v (%v)\n%s%s", jerr, err, out, stderr.String())
	}
	return doc, stderr.String()
}

func stateOf(doc statusDoc, localID string) string {
	for _, r := range doc.Resources {
		if r.LocalID == localID {
			return r.State
		}
	}
	return ""
}

// ---- テスト ----

func TestS3Integration(t *testing.T) {
	ctx := context.Background()
	// main は 4 つの API を持つサーバ（必須の seaweedfs と、使えれば minio）。
	var main []string
	for _, n := range []string{"seaweedfs", "minio"} {
		if _, ok := lab.servers[n]; ok {
			main = append(main, n)
		}
	}
	for name, why := range lab.skipped {
		t.Logf("%s (optional) is not used: %s", name, why)
	}
	n := len(main)
	writeIntent(t, appBuckets(main, false)...)

	t.Run("create from empty", func(t *testing.T) {
		s, _ := status(t)
		if s.Summary.Drift != n || !s.Complete {
			t.Fatalf("before: %+v, want %d drifted (the buckets do not exist yet)", s.Summary, n)
		}
		mustReconcile(t)
		s, errOut := status(t)
		if s.Summary.Conformant != n || s.Summary.Drift != 0 {
			t.Fatalf("after: %+v, want %d conformant\n%s", s, n, errOut)
		}
		for _, ep := range main {
			sess := session(t, ep)
			if v, err := sess.GetVersioning(ctx, "sk-it-app"); err != nil || v != "Enabled" {
				t.Errorf("%s: versioning = %q, %v", ep, v, err)
			}
			pol, err := sess.GetPolicy(ctx, "sk-it-app")
			want, _ := s3bucket.CanonPolicy(policyFor("sk-it-app"))
			got, _ := s3bucket.CanonPolicy(pol)
			if err != nil || got != want {
				t.Errorf("%s: policy = %s, %v; want %s", ep, pol, err, want)
			}
			t.Logf("%s keeps the policy as %s", ep, strings.TrimSpace(pol))
		}
	})

	t.Run("a policy and versioning changed by hand are drift and come back", func(t *testing.T) {
		for _, ep := range main {
			sess := session(t, ep)
			if err := sess.PutVersioning(ctx, "sk-it-app", "Suspended"); err != nil {
				t.Fatal(err)
			}
			if err := sess.PutPolicy(ctx, "sk-it-app", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::sk-it-app/*"}]}`); err != nil {
				t.Fatal(err)
			}
		}
		s, _ := status(t)
		if s.Summary.Drift != n {
			t.Fatalf("status after the hand changes: %+v, want %d drifted", s.Summary, n)
		}
		out := mustReconcile(t)
		if !strings.Contains(out, "policy") {
			t.Logf("reconcile output:\n%s", out)
		}
		if s, _ := status(t); s.Summary.Conformant != n || s.Summary.Drift != 0 {
			t.Fatalf("status after reconcile: %+v", s.Summary)
		}
		for _, ep := range main {
			if v, _ := session(t, ep).GetVersioning(ctx, "sk-it-app"); v != "Enabled" {
				t.Errorf("%s: versioning = %q after reconcile", ep, v)
			}
		}
	})

	t.Run("declared as is, no drift however many cycles", func(t *testing.T) {
		before := map[string]string{}
		for _, ep := range main {
			before[ep], _ = session(t, ep).GetPolicy(ctx, "sk-it-app")
		}
		for i := 0; i < 5; i++ {
			out := mustReconcile(t)
			if strings.Contains(out, "will create") || strings.Contains(out, "out of sync") {
				t.Fatalf("cycle %d planned a change:\n%s", i, out)
			}
			if s, _ := status(t); s.Summary.Drift != 0 || s.Summary.Conformant != n || !s.Complete {
				t.Fatalf("cycle %d: %+v", i, s.Summary)
			}
		}
		for _, ep := range main {
			if after, _ := session(t, ep).GetPolicy(ctx, "sk-it-app"); after != before[ep] {
				t.Errorf("%s: the policy was rewritten over the cycles:\n%s\n%s", ep, before[ep], after)
			}
		}
	})

	t.Run("a bucket with objects is not pruned", func(t *testing.T) {
		for _, ep := range main {
			sess := session(t, ep)
			for _, b := range []string{"sk-it-orphan-full", "sk-it-orphan-empty"} {
				if err := sess.CreateBucket(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			putObject(t, ep, "sk-it-orphan-full", "data/object.txt")
			if err := sess.CreateBucket(ctx, "other-not-managed"); err != nil { // bucket_prefix の外。
				t.Fatal(err)
			}
		}
		writeIntent(t, appBuckets(main, true)...)
		out, _ := sakutei(t, "reconcile")
		t.Logf("reconcile with PruneWithinScope:\n%s", out)
		for _, ep := range main {
			if !hasBucket(t, ep, "sk-it-orphan-full") {
				t.Errorf("%s: the bucket with objects was deleted", ep)
			}
			if hasBucket(t, ep, "sk-it-orphan-empty") {
				t.Errorf("%s: the empty orphan bucket is still there", ep)
			}
			if !hasBucket(t, ep, "other-not-managed") {
				t.Errorf("%s: a bucket outside bucket_prefix was deleted", ep)
			}
			if !hasBucket(t, ep, "sk-it-app") {
				t.Errorf("%s: the declared bucket was deleted", ep)
			}
		}
		if !strings.Contains(out, "BucketNotEmpty") || !strings.Contains(out, "not empty") {
			t.Errorf("reconcile did not report BucketNotEmpty:\n%s", out)
		}
	})

	// 止めるサーバ（paused）と、動いたままのサーバ（running）。minio が使えれば
	// minio を止めて seaweedfs を残す。使えなければ seaweedfs を止め、残すのは
	// seaweedfs-old（それも無ければ残すサーバの段は飛ばす）。
	paused, running := "minio", "seaweedfs"
	if _, ok := lab.servers["minio"]; !ok {
		paused, running = "seaweedfs", ""
		if _, ok := lab.servers["seaweedfs-old"]; ok {
			running = "seaweedfs-old"
		}
	}

	t.Run("a stopped server is unobservable and nothing is done", func(t *testing.T) {
		// running に空の orphan を置き、paused を止める。List が完全でないので
		// prune は止まり、paused のリソースは観測できない（absent にしない）。
		// running のリソースは観測を続ける。
		if running != "" {
			if err := session(t, running).CreateBucket(ctx, "sk-it-orphan-2"); err != nil {
				t.Fatal(err)
			}
		} else {
			t.Logf("no other server is running (%s, %s); only the stopped one is checked", lab.skipped["minio"], lab.skipped["seaweedfs-old"])
		}
		writeIntent(t, appBuckets(main, true)...)
		if out, err := docker("pause", lab.servers[paused].container); err != nil {
			t.Fatalf("docker pause: %v: %s", err, out)
		}
		defer func() {
			if out, err := docker("unpause", lab.servers[paused].container); err != nil {
				t.Fatalf("docker unpause: %v: %s", err, out)
			}
		}()
		s, _ := status(t)
		if got := stateOf(s, paused+"/sk-it-app"); s.Summary.Unobservable != 1 || got == "" || got == "conformant" || got == "drift" {
			t.Fatalf("status with %s stopped: %+v (%s/sk-it-app: %q), want it unobservable", paused, s.Summary, paused, got)
		}
		out, _ := sakutei(t, "reconcile")
		t.Logf("reconcile with %s stopped:\n%s", paused, out)
		if strings.Contains(out, "will create") {
			t.Errorf("reconcile planned to create a bucket it could not observe:\n%s", out)
		}
		if !strings.Contains(out, "skip "+paused+"/sk-it-app") {
			t.Errorf("the bucket on the stopped %s was not reported as skipped:\n%s", paused, out)
		}
		if running == "" {
			return
		}
		if !hasBucket(t, running, "sk-it-orphan-2") {
			t.Error("prune ran while one endpoint could not be listed")
		}
		// 止まっていないサーバのバケットは観測を続ける（skip しない）。
		if strings.Contains(out, "skip "+running+"/") {
			t.Errorf("a bucket on the running %s was skipped:\n%s", running, out)
		}
	})

	t.Run("after the server is back it converges again", func(t *testing.T) {
		if err := waitReady(paused, time.Minute); err != nil {
			t.Fatal(err)
		}
		writeIntent(t, appBuckets(main, false)...)
		mustReconcile(t)
		if s, _ := status(t); s.Summary.Conformant != n || s.Summary.Drift != 0 {
			t.Fatalf("%+v", s.Summary)
		}
	})

	t.Run("an endpoint without the API", func(t *testing.T) {
		need(t, "seaweedfs-old")
		// 今の版の minio・seaweedfs は 4 つの API の GET をどれも持つ（NotImplemented
		// にならない）。ここで確かめた結果を記録する。
		for _, ep := range main {
			for _, api := range []func(*s3bucket.Session) error{
				func(s *s3bucket.Session) error { _, err := s.GetVersioning(ctx, "sk-it-app"); return err },
				func(s *s3bucket.Session) error { _, err := s.GetPolicy(ctx, "sk-it-app"); return err },
				func(s *s3bucket.Session) error { _, err := s.GetLifecycle(ctx, "sk-it-app"); return err },
				func(s *s3bucket.Session) error { _, err := s.GetTagging(ctx, "sk-it-app"); return err },
			} {
				if err := api(session(t, ep)); err != nil {
					t.Errorf("%s: %v", ep, err)
				}
			}
		}
		// 古い seaweedfs はバージョニング・ポリシー・タグの GET に既定の値を返す
		// が、PUT は 501 NotImplemented。GET が NotImplemented でないので Read は
		// 欠けを知れず、宣言すれば drift になり、Apply が NotImplemented と分かる
		// 文の PermanentError で止まる（宣言していなければ無視される）。
		old := resource{Provider: s3bucket.Name, ID: "seaweedfs-old/sk-it-old", Fields: map[string]string{"versioning": "Enabled", "tags": `{"env": "integration"}`}}
		writeIntent(t, appBuckets(main, false, old)...)
		out, _ := sakutei(t, "reconcile")
		t.Logf("reconcile against seaweedfs-old:\n%s", out)
		if !strings.Contains(out, "does not implement this API (NotImplemented)") {
			t.Errorf("reconcile did not say the endpoint lacks the API:\n%s", out)
		}
		if !hasBucket(t, "seaweedfs-old", "sk-it-old") {
			t.Error("the bucket itself was not created on seaweedfs-old")
		}
		s, _ := status(t)
		if got := stateOf(s, "seaweedfs-old/sk-it-old"); got != "drift" {
			t.Errorf("seaweedfs-old/sk-it-old: %q, want drift (the declared values are not there)", got)
		}
		// 宣言しなければ無視される（バケットだけなら適合）。
		writeIntent(t, appBuckets(main, false, resource{Provider: s3bucket.Name, ID: "seaweedfs-old/sk-it-old", Fields: map[string]string{}})...)
		if s, _ := status(t); stateOf(s, "seaweedfs-old/sk-it-old") != "conformant" {
			t.Errorf("undeclared fields on seaweedfs-old: %+v", s)
		}
	})

	t.Run("fields leave out the settings the endpoint is not asked about", func(t *testing.T) {
		// seaweedfs-compat は fields が policy・tags だけ。versioning・lifecycle は
		// 読まず（unsupported と同じ扱い）、宣言しなければ適合、宣言すれば読み込みの
		// 時点で「この endpoint は扱わない」と分かるエラー。
		compat := resource{Provider: s3bucket.Name, ID: "seaweedfs-compat/sk-compat-app", Fields: map[string]string{
			"policy": policyFor("sk-compat-app"),
			"tags":   `{"env": "integration"}`,
		}}
		writeIntent(t, appBuckets(main, false, compat)...)
		mustReconcile(t)
		s, errOut := status(t)
		if got := stateOf(s, "seaweedfs-compat/sk-compat-app"); got != "conformant" {
			t.Fatalf("seaweedfs-compat/sk-compat-app: %q, want conformant\n%+v\n%s", got, s, errOut)
		}
		// 同じバケットは seaweedfs の側（bucket_prefix sk-it-）には見えない。
		if got := stateOf(s, "seaweedfs/sk-compat-app"); got != "" {
			t.Errorf("seaweedfs/sk-compat-app is reported (%q); the prefixes must not overlap", got)
		}
		// versioning は既定のまま（触らない）。
		if v, err := session(t, "seaweedfs").GetVersioning(ctx, "sk-compat-app"); err != nil || v == "Enabled" {
			t.Errorf("versioning of sk-compat-app = %q, %v; want untouched", v, err)
		}
		compat.Fields["versioning"] = "Enabled"
		writeIntent(t, appBuckets(main, false, compat)...)
		out, code := sakutei(t, "plan")
		if code == 0 || !strings.Contains(out, "cannot manage versioning") || !strings.Contains(out, "fields") {
			t.Errorf("plan with versioning declared on seaweedfs-compat exited %d; want an error naming the endpoint's fields:\n%s", code, out)
		}
		// lifecycle の StorageClass の読み替え（storage_class_aliases）は、この版の
		// SeaweedFS では再現できない（Transition を含む lifecycle の PUT は 501
		// NotImplemented、seaweedfs-old は受け付けるが読み戻すと Transition が
		// 無い）。読み替えは偽サーバの単体・受け入れテストで確かめる。
		writeIntent(t, appBuckets(main, false)...)
	})

	t.Run("clean up through prune", func(t *testing.T) {
		// 宣言を 1 つだけ残して PruneWithinScope にすると、空のバケットは prune で
		// 消える（中身のあるものは残る）。コンテナは teardown で消す。
		writeIntent(t, resource{Provider: s3bucket.Name, ID: "seaweedfs/sk-it-keep", Fields: map[string]string{}, DeletionPolicy: "PruneWithinScope"})
		out, _ := sakutei(t, "reconcile")
		t.Logf("final reconcile:\n%s", out)
	})
}

func sha256sum(b []byte) [32]byte { return sha256Sum(b) }

var sha256Sum = sha256.Sum256
