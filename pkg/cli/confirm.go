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

package cli

// confirm.go — 変えたあとの確かめの関数を名前で足す口（WithConfirmCheck、
// Backlog #200、§23.4）と、読み込んだ intent の Confirmer を provider に渡す所。

import (
	"fmt"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// WithConfirmCheck は確かめの関数を名前で足す（#200）。confirm の先に
// check:<name> と書いたリソース（今は netconf の部分木）は、変えたあと
// （commit confirmed のあと、確定の前）に f が nil を返すまで、時間の上限の
// 中で f を繰り返し呼び、nil にならなければ確定しない。何をどう読んで判定
// するかは f が持つ（f の ctx は観測の文脈: 書き込み用の資格は使わない）。
// 同じ名前を 2 度足すと起動のエラー。network_policy の realizer は Output の
// Checks でも渡せる（名前はここのものと重ならないこと）。
func WithConfirmCheck(name string, f provider.ConfirmCheck) Option {
	return func(o *options) { o.confirmChecks = append(o.confirmChecks, namedConfirmCheck{name, f}) }
}

type namedConfirmCheck struct {
	name string
	f    provider.ConfirmCheck
}

// baseConfirmChecks are the check functions of WithConfirmCheck (set by Main).
var baseConfirmChecks map[string]provider.ConfirmCheck

// registerConfirmChecks は Main の起動で WithConfirmCheck の関数を足す。
func registerConfirmChecks(cs []namedConfirmCheck) error {
	out := map[string]provider.ConfirmCheck{}
	for _, c := range cs {
		if err := provider.ValidConfirmCheckName(c.name); err != nil || c.f == nil {
			return fmt.Errorf("confirm check %q: want a name of letters, digits, '_', '.', '-' and a non-nil function", c.name)
		}
		if _, dup := out[c.name]; dup {
			return fmt.Errorf("confirm check %q is registered twice", c.name)
		}
		out[c.name] = c.f
	}
	baseConfirmChecks = out
	return nil
}

// installConfirmer は読み込んだ intent の Confirmer を providers に渡す
// （ValidateIntent の前に）。
func installConfirmer(np *netpolicy.Result, resources []diff.DesiredResource, providers map[string]provider.Provider) error {
	checks, err := reconcile.ConfirmChecks(baseConfirmChecks, np)
	if err != nil {
		return err
	}
	reconcile.InstallConfirmer(resources, func(name string) provider.Provider { return providers[name] }, checks)
	return nil
}
