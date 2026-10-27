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

// template_file_test.go — #139: 自動導入の応答ファイルを宣言から作る
// （template-file provider）を、本物の CLI（plan・reconcile・status）と
// --provider-config で確かめる。雛形は試用と統合テストが使う
// docs/trial/redfish-os-install/templates/alpine-answers.sh.tmpl。
//
//	[AC-139-8]  宣言（ホスト名・アドレス・公開鍵・目印）から応答ファイルが書き出され、
//	            もう一度回すと適合。手で直されたら status が drift と出し、reconcile が
//	            宣言どおりに書き直す。plan はファイルを書かない。
//	[AC-139-9]  秘密（SecretRef・秘密鍵）は書き出せない: 読み込みの時点で断り、何も書かない。
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const answerUUID = "6f1b7c52-0e1a-4d2b-9c11-000000008501"

func templateFileRig(t *testing.T, fields string) (bin string, common []string, out string) {
	t.Helper()
	bin = buildBinary(t)
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `{"resources": [{"provider": "template-file", "id": "` + answerUUID + `", "fields": {` + fields + `}}]}`
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	templates, err := filepath.Abs("../../docs/trial/redfish-os-install/templates")
	if err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(tmp, "answers")
	pc := filepath.Join(tmp, "provider-config.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{
		"template-file": map[string]any{"template_dir": templates, "output_dir": out},
	}})
	return bin, []string{"--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc}, out
}

func TestTemplateFile_AnswerFileFromIntent(t *testing.T) {
	bin, common, out := templateFileRig(t, `"template": "alpine-answers.sh.tmpl", "hostname": "node1",
	  "address": "10.0.2.15/24", "var.gateway": "10.0.2.2", "install_mark": "install-1",
	  "ssh_authorized_keys": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample sakutei"`)
	file := filepath.Join(out, answerUUID)

	// [AC-139-8] plan は書かない。
	if o, code := runBin(t, bin, append([]string{"plan"}, common...)...); code != 0 || !strings.Contains(o, "template-file//"+answerUUID) {
		t.Fatalf("[AC-139-8] plan exit %d:\n%s", code, o)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatalf("[AC-139-8] plan wrote %s", file)
	}

	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "1 change(s) applied") {
		t.Fatalf("[AC-139-8] reconcile exit %d:\n%s", code, o)
	}
	got := readFile(t, file)
	for _, want := range []string{"HOSTNAME='node1'", "ADDRESS='10.0.2.15/24'", "GATEWAY='10.0.2.2'", "MARK='install-1'",
		"SSH_AUTHORIZED_KEYS='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample sakutei'"} {
		if !strings.Contains(got, want) {
			t.Errorf("[AC-139-8] answer file lacks %q:\n%s", want, got)
		}
	}
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "Converged (no changes)") {
		t.Fatalf("[AC-139-8] 2nd reconcile exit %d, want converged:\n%s", code, o)
	}

	// 手で直された → status が drift、reconcile が書き直す。
	if err := os.WriteFile(file, []byte("HOSTNAME='evil'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, _ := runBin(t, bin, append([]string{"status"}, common...)...); !strings.Contains(o, "differs from the rendered template") {
		t.Errorf("[AC-139-8] status after a hand edit:\n%s", o)
	}
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "1 change(s) applied") {
		t.Fatalf("[AC-139-8] reconcile after a hand edit exit %d:\n%s", code, o)
	}
	if got := readFile(t, file); !strings.Contains(got, "HOSTNAME='node1'") {
		t.Errorf("[AC-139-8] not rewritten:\n%s", got)
	}
}

func TestTemplateFile_RefusesSecrets(t *testing.T) {
	for _, c := range []struct{ fields, want string }{
		{`"template": "alpine-answers.sh.tmpl", "var.password": "secretref:file/root"`, "SecretRef"},
		{`"template": "alpine-answers.sh.tmpl", "ssh_authorized_keys": "-----BEGIN OPENSSH PRIVATE KEY-----"`, "private key"},
	} {
		bin, common, out := templateFileRig(t, c.fields)
		o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...)
		if code == 0 || !strings.Contains(o, c.want) {
			t.Errorf("[AC-139-9] reconcile with %s: exit %d, want a refusal mentioning %q:\n%s", c.fields, code, c.want, o)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("[AC-139-9] something was written to %s", out)
		}
	}
}
