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

//go:build trial

// bulkeq は試用（docs/trial/observe/README.md）の道具：いまのゾーンファイルと
// ruleset に対して、BulkObserve の結果が per-resource Read と等しいことを SDK の
// BulkEquivalenceTester（§5.7・§28.3）で確かめ、両方の観測を並べて出す。
// 現実のファイルを読むだけで書かない。
//
//	go run -tags trial ./docs/trial/observe/bulkeq --zone <file> --ruleset <file>
//
// 等しくなければ違いを出して exit 1。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/internal/nftables"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// report は sdk.TB を満たし、違反を数えて出す。
type report struct {
	name   string
	failed int
}

func (r *report) Helper() {}
func (r *report) Errorf(format string, args ...any) {
	r.failed++
	fmt.Printf("  NG %s: %s\n", r.name, fmt.Sprintf(format, args...))
}
func (r *report) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	panic(r)
}
func (r *report) Logf(format string, args ...any) {
	fmt.Printf("  %s: %s\n", r.name, fmt.Sprintf(format, args...))
}

type bulkProvider interface {
	provider.Provider
	provider.Lister
	provider.BulkObserver
}

func check(name string, p bulkProvider) (failed int) {
	r := &report{name: name}
	defer func() {
		if v := recover(); v != nil && v != r {
			panic(v)
		}
		failed = r.failed
	}()
	ctx := context.Background()
	root := core.NewScope(nil)
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: root}
	bt.Run(r)

	// 並べて見せる：BulkObserve の 1 回と、ID ごとの Read。
	bulk, err := p.BulkObserve(ctx, root)
	if err != nil {
		r.Fatalf("BulkObserve: %v", err)
	}
	lr, err := p.List(ctx, root)
	if err != nil {
		r.Fatalf("List: %v", err)
	}
	ids := append([]core.ID(nil), lr.IDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i].Local < ids[j].Local })
	same := 0
	for _, id := range ids {
		o := bulk.Observations[id.Key()]
		ref, err := p.Read(ctx, id.Local)
		if err != nil {
			r.Fatalf("Read(%s): %v", id.Local, err)
		}
		mark := "!="
		if fmt.Sprint(o.State) == fmt.Sprint(ref.State) && o.Availability == ref.Availability {
			mark = "=="
			same++
		}
		fmt.Printf("  %s\n      bulk %s\n   %s read %s\n", id.Local, show(o.State), mark, show(ref.State))
	}
	if len(bulk.Observations) != len(ids) {
		r.Errorf("BulkObserve returned %d, List %d", len(bulk.Observations), len(ids))
	}
	fmt.Printf("%s: BulkObserve と Read が等しい: %d/%d（違反 %d）\n", name, same, len(ids), r.failed)
	return r.failed
}

// show は State を「名前=値」を名前順に並べた 1 行にする。
func show(st core.State) string {
	names := make([]string, 0, len(st))
	for n := range st {
		names = append(names, n)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=%q", n, fmt.Sprint(st[n].Value))
	}
	return out
}

func main() {
	zone := flag.String("zone", "", "dnszone のゾーンファイル")
	ruleset := flag.String("ruleset", "", "nftables の ruleset ファイル")
	flag.Parse()
	failed := 0
	if *zone != "" {
		failed += check("dnszone", dnszone.New(*zone))
	}
	if *ruleset != "" {
		failed += check("nftables", nftables.New(*ruleset))
	}
	if failed > 0 {
		os.Exit(1)
	}
}
