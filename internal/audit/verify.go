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

import "fmt"

// Problem は Verify が見つけた切れ目・書き換えの 1 つ。
type Problem struct {
	File string `json:"file"`
	Line int    `json:"line,omitempty"` // ファイルの中の行番号（1 から）
	Seq  int64  `json:"seq,omitempty"`
	Msg  string `json:"message"`
}

// Anchor は外に出した記録（送り先の状態など）で分かっている 1 行：通し番号と
// その行のハッシュ。ログがそこまで届いていない、またはその行が違えば切り詰め・
// 書き換えである（鎖だけでは末尾の切り詰めは分からない）。
type Anchor struct {
	Seq  int64
	Hash string
	From string // どこで分かったか（知らせに出す）
}

// Report は Verify の結果。
type Report struct {
	Files   []string `json:"files"`
	Records int64    `json:"records"`
	// Legacy は鎖より前の版で書いた（seq・prev の無い）先頭の行の数。
	Legacy int64 `json:"legacy,omitempty"`
	// LastSeq と Head は最後の行の通し番号とハッシュ（外に控えておけば、次の
	// verify の Anchor になる）。
	LastSeq  int64     `json:"last_seq"`
	Head     string    `json:"head,omitempty"`
	Problems []Problem `json:"problems,omitempty"`
}

// OK は切れ目・書き換えが見つからなかったか。
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify は path（回したあとの続きのファイルを含む）の鎖を確かめる（#187）。
// 読めない行・通し番号の飛び・前の行のハッシュの食い違い・抜けたファイル・鎖が
// 始まったあとの seq の無い行・anchors との食い違いを Problems に返す。
// error はファイルを読めなかったときだけ。
func Verify(path string, anchors ...Anchor) (Report, error) {
	var r Report
	segs, err := segments(path)
	if err != nil {
		return r, err
	}
	for i, sg := range segs {
		r.Files = append(r.Files, segmentPath(path, sg))
		if sg != i {
			r.Problems = append(r.Problems, Problem{File: segmentPath(path, i),
				Msg: "this file is missing (a rotated file was removed); the records in it are lost"})
			break
		}
	}
	want := map[int64]Anchor{}
	for _, a := range anchors {
		want[a.Seq] = a
	}
	var (
		prevHash string
		chained  bool
		lines    int64 // 空でない行の数（通し番号はこれに合う）
		next     int64 // 鎖の次に来るはずの通し番号
	)
	add := func(l rawLine, seq int64, msg string, args ...any) {
		r.Problems = append(r.Problems, Problem{File: l.file, Line: l.n, Seq: seq, Msg: fmt.Sprintf(msg, args...)})
	}
	err = scan(path, 0, 0, func(l rawLine) error {
		lines++
		h := lineHash(l.raw)
		defer func() { prevHash = h }()
		switch {
		case l.err != nil:
			add(l, 0, "not a JSON record (a torn write, or the line was edited): %v", l.err)
			next++
			return nil
		case l.rec.Seq == 0:
			if chained {
				add(l, 0, "a record without seq / prev after the chain began (inserted, or its seq / prev were stripped)")
				next++
				return nil
			}
			r.Legacy++
			return nil
		}
		if !chained {
			chained = true
			next = lines
		}
		if l.rec.Seq != next {
			add(l, l.rec.Seq, "seq %d where %d was expected (a record was removed or inserted before it)", l.rec.Seq, next)
		}
		if l.rec.Prev != prevHash {
			add(l, l.rec.Seq, "prev does not match the record before it (that record was rewritten, removed or inserted)")
		}
		if a, ok := want[l.rec.Seq]; ok {
			if a.Hash != "" && a.Hash != h {
				add(l, l.rec.Seq, "this record differs from what was sent out (%s): it was rewritten", a.From)
			}
			delete(want, l.rec.Seq)
		}
		next = l.rec.Seq + 1
		r.LastSeq = l.rec.Seq
		return nil
	})
	if err != nil {
		return r, err
	}
	r.Records = lines
	if lines > 0 {
		r.Head = prevHash
		if !chained {
			r.LastSeq = lines
		}
	}
	for _, a := range anchors {
		if _, left := want[a.Seq]; left && a.Seq > r.LastSeq {
			r.Problems = append(r.Problems, Problem{File: path, Seq: a.Seq, Msg: fmt.Sprintf(
				"the log ends at seq %d but seq %d was already sent out (%s): the log was truncated", r.LastSeq, a.Seq, a.From)})
		}
	}
	return r, nil
}
