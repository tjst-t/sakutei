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

package audit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Append(Record{Kind: KindApply, Resource: fmt.Sprintf("fake//r%d", i), Result: ResultOK}); err != nil {
			t.Fatal(err)
		}
	}
}

func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
}

func writeLines(t *testing.T, path string, lines [][]byte) {
	t.Helper()
	if err := os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustVerify(t *testing.T, path string, anchors ...Anchor) Report {
	t.Helper()
	r, err := Verify(path, anchors...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAppend_Chains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	appendN(t, NewLog(path), 3)
	recs, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	for i, r := range recs {
		if r.Seq != int64(i+1) {
			t.Errorf("record %d: seq %d", i, r.Seq)
		}
		want := ""
		if i > 0 {
			want = lineHash(lines[i-1])
		}
		if r.Prev != want {
			t.Errorf("record %d: prev %q, want %q", i, r.Prev, want)
		}
	}
	r := mustVerify(t, path)
	if !r.OK() || r.Records != 3 || r.LastSeq != 3 || r.Head != lineHash(lines[2]) {
		t.Fatalf("verify = %+v", r)
	}
}

func TestVerify_FindsRewriteRemovalInsertion(t *testing.T) {
	cases := map[string]func([][]byte) [][]byte{
		"rewrite": func(l [][]byte) [][]byte {
			l[1] = bytes.Replace(l[1], []byte("fake//r1"), []byte("fake//rX"), 1)
			return l
		},
		"remove": func(l [][]byte) [][]byte { return append(l[:2:2], l[3:]...) },
		"insert": func(l [][]byte) [][]byte {
			out := append([][]byte{}, l[:2]...)
			out = append(out, []byte(`{"time":"2026-01-01T00:00:00Z","kind":"resource.apply","resource":"fake//evil"}`))
			return append(out, l[2:]...)
		},
		"edit the last but one": func(l [][]byte) [][]byte {
			l[len(l)-2] = bytes.Replace(l[len(l)-2], []byte(`"ok"`), []byte(`"failed"`), 1)
			return l
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.jsonl")
			appendN(t, NewLog(path), 5)
			writeLines(t, path, tamper(readLines(t, path)))
			if r := mustVerify(t, path); r.OK() {
				t.Fatalf("verify did not find the %s: %+v", name, r)
			}
		})
	}
}

// 末尾の切り詰めは鎖だけでは分からない。外へ送った所（Anchor）で見つける。
func TestVerify_TruncationNeedsAnAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	appendN(t, NewLog(path), 4)
	lines := readLines(t, path)
	anchor := Anchor{Seq: 4, Hash: lineHash(lines[3]), From: "test"}
	if r := mustVerify(t, path, anchor); !r.OK() {
		t.Fatalf("intact log with its anchor: %+v", r)
	}
	writeLines(t, path, lines[:2])
	if r := mustVerify(t, path); !r.OK() {
		t.Fatalf("a truncated chain is still a chain without an anchor: %+v", r)
	}
	r := mustVerify(t, path, anchor)
	if r.OK() || !strings.Contains(r.Problems[0].Msg, "truncated") {
		t.Fatalf("verify with the anchor = %+v, want truncated", r)
	}
	// 送ったあとに最後の行を書き換えても、anchor で分かる。
	writeLines(t, path, append(lines[:3:3], bytes.Replace(lines[3], []byte("r3"), []byte("rZ"), 1)))
	if r := mustVerify(t, path, anchor); r.OK() {
		t.Fatalf("a rewritten last line passed with its anchor: %+v", r)
	}
}

func TestRotate_NewFileAndChainAcross(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l := NewLog(path)
	l.MaxBytes = 400
	appendN(t, l, 10)
	segs, err := segments(path)
	if err != nil || len(segs) < 3 {
		t.Fatalf("segments = %v, %v; want rotation", segs, err)
	}
	first, _ := os.ReadFile(path)
	appendN(t, l, 2)
	again, _ := os.ReadFile(path)
	if !bytes.Equal(first, again) {
		t.Fatal("the first file changed after it was rotated")
	}
	recs, err := ReadAll(path)
	if err != nil || len(recs) != 12 || recs[11].Seq != 12 {
		t.Fatalf("ReadAll across files: %d records, %v", len(recs), err)
	}
	if r := mustVerify(t, path); !r.OK() || r.Records != 12 || len(r.Files) < len(segs) {
		t.Fatalf("verify across files = %+v", r)
	}
	// 回したファイルを 1 つ消すと見つかる。
	if err := os.Remove(segmentPath(path, 1)); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, path); r.OK() {
		t.Fatalf("a removed rotated file passed: %+v", r)
	}
}

// 鎖より前の版で書いた行は先頭にあれば legacy として受け、鎖はそのあとに続く。
func TestAppend_AfterLegacyRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	legacy := []byte(`{"time":"2026-01-01T00:00:00Z","kind":"freeze.started","actor":"alice"}`)
	writeLines(t, path, [][]byte{legacy, legacy})
	appendN(t, NewLog(path), 2)
	recs, _ := ReadAll(path)
	if recs[2].Seq != 3 || recs[2].Prev != lineHash(legacy) {
		t.Fatalf("first chained record = %+v", recs[2])
	}
	r := mustVerify(t, path)
	if !r.OK() || r.Legacy != 2 || r.LastSeq != 4 {
		t.Fatalf("verify = %+v", r)
	}
}

// 書いている途中で切れた行は切り詰めずに閉じ、verify が知らせる。
func TestAppend_AfterTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l := NewLog(path)
	appendN(t, l, 1)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":2,"time":"2026-`)
	f.Close()
	appendN(t, l, 1)
	lines := readLines(t, path)
	if len(lines) != 3 || !strings.HasPrefix(string(lines[2]), `{"seq":3,`) {
		t.Fatalf("lines = %q", lines)
	}
	if r := mustVerify(t, path); r.OK() || r.Problems[0].Line != 2 {
		t.Fatalf("verify = %+v, want the torn line 2", r)
	}
}

// 複数の書き手（別々の Log）でも鎖は切れない。
func TestAppend_ConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := NewLog(path)
			l.MaxBytes = 2000
			for i := 0; i < 25; i++ {
				if err := l.Append(Record{Kind: KindApply, Resource: "fake//c"}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if r := mustVerify(t, path); !r.OK() || r.Records != 100 || r.LastSeq != 100 {
		t.Fatalf("verify = %+v", r)
	}
}
