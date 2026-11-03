package acceptance

// migrate_trial_test.go — v2 の試用の intent（docs/trial/）に対する v2 の plan の
// 操作を確かめる（Backlog #224、新しいコアの設計 §8 の段 9）。同じ表
// （v3/providers/migrate/testdata/trial-cases.json）を新しいコアの側の
// v3/providers/migrate の試験が sakutei-migrate で書き直して回し、新しいコアの plan が
// 同じ操作を出すかを確かめる。表の ops はこの試験が v2 の plan と突き合わせる。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// trialCase は trial-cases.json の 1 件（v3/providers/migrate/trial_test.go と同じ形）。
type trialCase struct {
	Name       string            `json:"name"`
	Intent     string            `json:"intent"`
	Files      map[string]string `json:"files"`
	Replace    []struct{ File, Old, New string }
	Write      map[string]string `json:"write"`
	Flags      []string          `json:"flags"`
	Ops        []string          `json:"ops"`
	Procedures []string          `json:"procedures"`
	Unknown    []string          `json:"unknown"`
}

func TestMigrateTrialV2Plan(t *testing.T) {
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	data, err := os.ReadFile(filepath.Join(root, "v3", "providers", "migrate", "testdata", "trial-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []trialCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	bin := buildBinary(t)
	for _, tc := range table.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			w := t.TempDir()
			copyTree(t, filepath.Join(root, tc.Intent), filepath.Join(w, "intent"))
			for dst, src := range tc.Files {
				copyTree(t, filepath.Join(root, src), filepath.Join(w, dst))
			}
			for _, r := range tc.Replace {
				p := filepath.Join(w, r.File)
				b, err := os.ReadFile(p)
				if err != nil || !strings.Contains(string(b), r.Old) {
					t.Fatalf("%s に %q が無い（%v）", r.File, r.Old, err)
				}
				os.WriteFile(p, []byte(strings.ReplaceAll(string(b), r.Old, r.New)), 0o600)
			}
			for name, s := range tc.Write {
				os.MkdirAll(filepath.Dir(filepath.Join(w, name)), 0o755)
				os.WriteFile(filepath.Join(w, name), []byte(s), 0o600)
			}
			args := []string{"plan", "--intent", filepath.Join(w, "intent"), "--json"}
			for _, f := range tc.Flags {
				if strings.HasPrefix(f, "@") {
					f = filepath.Join(w, f[1:])
				}
				args = append(args, f)
			}
			cmd := exec.Command(bin, args...)
			cmd.Dir = w
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("plan: %v\n%s", err, out)
			}
			var p struct {
				Operations []struct {
					ID      string          `json:"id"`
					Kind    string          `json:"kind"`
					Blocked bool            `json:"blocked"`
					Task    json.RawMessage `json:"task"`
				} `json:"operations"`
				Workflow     []struct{ ID string } `json:"workflow"`
				Unobservable []struct{ ID string } `json:"unobservable"`
			}
			if err := json.Unmarshal(out, &p); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			var ops, procs, unknown []string
			for _, o := range p.Operations {
				if len(o.Task) > 0 {
					procs = append(procs, o.ID)
					continue
				}
				s := strings.ToLower(o.Kind) + " " + o.ID
				if o.Blocked {
					s += " (blocked)"
				}
				ops = append(ops, s)
			}
			for _, w := range p.Workflow {
				procs = append(procs, w.ID)
			}
			for _, u := range p.Unobservable {
				unknown = append(unknown, u.ID)
			}
			sameSet(t, "ops", ops, tc.Ops)
			sameSet(t, "procedures", procs, tc.Procedures)
			sameSet(t, "unknown", unknown, tc.Unknown)
		})
	}
}

func sameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	sort.Strings(got)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if strings.Join(got, "\n") != strings.Join(w, "\n") {
		t.Errorf("%s:\n got  %q\n want %q", what, got, w)
	}
}

// copyTree はファイルかディレクトリを写す。
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(to), 0o755)
		return os.WriteFile(to, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
