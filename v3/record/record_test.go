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

package record_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/record"
)

func open(t *testing.T) *record.Record {
	t.Helper()
	r, err := record.Open(filepath.Join(t.TempDir(), "record"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 進める処理は 1 つ（E-M5・N-M7）。放せば次が取れる。
func TestLock(t *testing.T) {
	r := open(t)
	release, held, err := r.Lock()
	if err != nil || !held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	other, _ := record.Open(r.Dir())
	if _, held2, err := other.Lock(); err != nil || held2 {
		t.Fatalf("2 つ目が取れた: held=%v err=%v", held2, err)
	}
	release()
	release2, held2, err := other.Lock()
	if err != nil || !held2 {
		t.Fatalf("放したあと取れない: held=%v err=%v", held2, err)
	}
	release2()
}

// 知らない形の版の Record は読まない（§5.1）。
func TestFormat(t *testing.T) {
	r := open(t)
	if data, _ := os.ReadFile(filepath.Join(r.Dir(), "format")); string(data) != "1\n" {
		t.Fatalf("format: %q", data)
	}
	if err := os.WriteFile(filepath.Join(r.Dir(), "format"), []byte("9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Open(r.Dir()); err == nil || !strings.Contains(err.Error(), `"9"`) {
		t.Fatalf("err=%v", err)
	}
	if _, err := record.ReadMarks(r.Dir()); err == nil {
		t.Fatal("知らない版の目印を読んだ")
	}
}

func TestMarks(t *testing.T) {
	r := open(t)
	m, err := r.Marks()
	if err != nil || m.Cycle != 0 || m.RangeCounts != nil {
		t.Fatalf("初めての周: %+v %v", m, err)
	}
	at := time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)
	want := record.Marks{Cycle: 3, At: at, RangeCounts: map[string]int{"dns:zone=example.com.": 412},
		BudgetWindows: map[string]record.WindowUse{"default": {Start: at, Destructive: 4}}}
	if err := r.WriteMarks(want); err != nil {
		t.Fatal(err)
	}
	got, err := record.ReadMarks(r.Dir())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v err=%v", got, err)
	}
	// 一時ファイルを残さない。
	ents, _ := os.ReadDir(r.Dir())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("一時ファイルが残った: %s", e.Name())
		}
	}
	// 無いディレクトリは作らず空の目印。
	missing := filepath.Join(t.TempDir(), "none")
	if m, err := record.ReadMarks(missing); err != nil || m.Cycle != 0 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("ReadMarks がディレクトリを作った")
	}
}

// 予算の窓は長さ分続き、過ぎれば 0 から数え直す（E-J1）。
func TestWindow(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	m := record.Marks{BudgetWindows: map[string]record.WindowUse{"p": {Start: start, Destructive: 4}}}
	if w := m.Window("p", 24*time.Hour, start.Add(23*time.Hour)); w.Destructive != 4 || !w.Start.Equal(start) {
		t.Fatalf("窓の中: %+v", w)
	}
	now := start.Add(24 * time.Hour)
	if w := m.Window("p", 24*time.Hour, now); w.Destructive != 0 || !w.Start.Equal(now) {
		t.Fatalf("窓のあと: %+v", w)
	}
	// 時計が窓の始まりより前に戻っても数え直さない（予算を緩めない）。
	if w := m.Window("p", 24*time.Hour, start.Add(-time.Hour)); w.Destructive != 4 {
		t.Fatalf("時計が戻った: %+v", w)
	}
	if w := m.Window("q", time.Hour, now); w.Destructive != 0 || !w.Start.Equal(now) {
		t.Fatalf("無い窓: %+v", w)
	}
}

// 監査の鎖：前の行のハッシュで鎖にし、書き換え・欠け・切れを audit verify が
// 見つける（E-L1・N-L1・不変条件 28）。
func TestAuditChain(t *testing.T) {
	r := open(t)
	a, err := r.Audit()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if seq, err := a.Append(record.Entry{Kind: "resource.apply", Resource: "dns/record/www", Fields: []string{"value"}, Result: "ok"}); err != nil || seq != uint64(i+1) {
			t.Fatalf("seq=%d err=%v", seq, err)
		}
	}
	// 開き直しても鎖が続く。
	a, err = r.Audit()
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := a.Append(record.Entry{Kind: "resource.prune", Resource: "dns/record/x", Result: "ok"}); err != nil || seq != 4 {
		t.Fatalf("seq=%d err=%v", seq, err)
	}
	if n, err := record.Verify(r.Dir()); n != 4 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}

	path := filepath.Join(r.Dir(), "audit", "000001.jsonl")
	orig, _ := os.ReadFile(path)
	// 2 行目の結果を書き換える。
	if err := os.WriteFile(path, bytes.Replace(orig, []byte(`"seq":2,`), []byte(`"seq":2,"x":1,`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Verify(r.Dir()); err == nil || !strings.Contains(err.Error(), "seq 3 の prev") {
		t.Fatalf("書き換えを見つけない: %v", err)
	}
	// 1 行を抜く。
	lines := strings.SplitAfter(string(orig), "\n")
	if err := os.WriteFile(path, []byte(lines[0]+lines[2]+lines[3]), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Verify(r.Dir()); err == nil || !strings.Contains(err.Error(), "seq 3（2 のはず）") {
		t.Fatalf("欠けを見つけない: %v", err)
	}
	// 切れた最後の行：verify も、続きを書くことも断る。
	if err := os.WriteFile(path, orig[:len(orig)-5], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Verify(r.Dir()); err == nil {
		t.Fatal("切れを見つけない")
	}
	if _, err := r.Audit(); err == nil {
		t.Fatal("切れた鎖に続きを書こうとした")
	}
}

// 大きくなれば新しいファイルへ回し、鎖はファイルをまたいで続く。
func TestAuditRotate(t *testing.T) {
	old := record.MaxAuditFile
	record.MaxAuditFile = 300
	defer func() { record.MaxAuditFile = old }()
	r := open(t)
	a, err := r.Audit()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := a.Append(record.Entry{Kind: "resource.apply", Resource: "dns/record/www", Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(r.Dir(), "audit"))
	if len(ents) < 3 {
		t.Fatalf("回していない: %d", len(ents))
	}
	if n, err := record.Verify(r.Dir()); n != 10 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	a, _ = r.Audit()
	if seq, err := a.Append(record.Entry{Kind: "resource.apply"}); seq != 11 || err != nil {
		t.Fatalf("seq=%d err=%v", seq, err)
	}
	// 途中のファイルが無ければ切れ目。
	if err := os.Remove(filepath.Join(r.Dir(), "audit", "000002.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Verify(r.Dir()); err == nil || !strings.Contains(err.Error(), "000002.jsonl が無い") {
		t.Fatalf("err=%v", err)
	}
}
