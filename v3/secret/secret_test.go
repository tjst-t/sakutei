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

package secret_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/secret"
)

// TestFile は組み込みの保管先 file：世代は中身を読まずに変わり、周の間に世代が
// 変われば値を返さない（E-G2・E-G3）。
func TestFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	os.WriteFile(path, []byte("v1-secret\n"), 0o600)
	b, err := secret.FileModule(map[string]any{"dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	g1, err := b.Generation(ctx, "token")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := b.Resolve(ctx, "token", g1); err != nil || v != "v1-secret" {
		t.Fatalf("Resolve = %q, %v", v, err)
	}
	os.WriteFile(path, []byte("v2-secret-longer\n"), 0o600)
	os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	g2, _ := b.Generation(ctx, "token")
	if g2 == g1 {
		t.Fatal("中身が変わっても世代が変わらない")
	}
	if _, err := b.Resolve(ctx, "token", g1); err == nil || strings.Contains(err.Error(), "v2-secret") {
		t.Fatalf("古い世代で値を返した・誤りに値を出した: %v", err)
	}
	if _, err := b.Generation(ctx, "../etc/passwd"); err == nil {
		t.Fatal("dir の外を読んだ")
	}
	if _, err := secret.FileModule(map[string]any{"path": "x"}); err == nil {
		t.Fatal("知らない設定を断らない")
	}
}

// TestStamp は宣言の Secret の項目に世代を付け、読めなければ Uncertain、平文は
// Reject にする（E-G4・不変条件 27）。
func TestStamp(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok"), []byte("x"), 0o600)
	backends := core.SecretBackends{"f": &secret.File{Dir: dir}}
	kind := func(core.ID) (core.KindSchema, bool) {
		return core.KindSchema{Fields: map[string]core.FieldSpec{"pw": {Type: core.Secret}}}, true
	}
	res := []core.Resource{
		{ID: core.ID{Type: "db/user", Name: "a"}, Fields: core.Fields{"pw": map[string]any{"secret": "f:ok"}}},
		{ID: core.ID{Type: "db/user", Name: "b"}, Fields: core.Fields{"pw": map[string]any{"secret": "f:missing"}}},
		{ID: core.ID{Type: "db/user", Name: "c"}, Fields: core.Fields{"pw": "plain-text-password"}},
		{ID: core.ID{Type: "db/user", Name: "d"}, Fields: core.Fields{"pw": map[string]any{"secret": "nope:x"}}},
	}
	out, diags := secret.Stamp(context.Background(), res, kind, backends)
	if s, _ := out[0].Fields["pw"].(string); !strings.HasPrefix(s, "f:ok@gen=") {
		t.Fatalf("世代を付けていない: %v", out[0].Fields)
	}
	sev := map[string]core.Severity{}
	for _, d := range diags {
		sev[d.Target] = d.Severity
		if strings.Contains(d.Message, "plain-text-password") {
			t.Errorf("診断に平文を出した: %s", d.Message)
		}
	}
	if sev["db/user/b"] != core.Uncertain || sev["db/user/c"] != core.Reject || sev["db/user/d"] != core.Reject || len(diags) != 3 {
		t.Fatalf("診断 = %v", diags)
	}
}
