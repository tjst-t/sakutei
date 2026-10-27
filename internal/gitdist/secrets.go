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

package gitdist

import (
	"fmt"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// CheckSecretLocality は拠点ローカルの file SecretBackend（store）が claim の拠点の
// 資格だけを持つことを確かめる（§20.5 資格局所性）。ほかの拠点の資格が 1 つでも
// あればエラーにする——中央の全拠点資格を拠点に置くと、1 拠点の漏洩が全拠点に及ぶ。
// 返すエラーにはパスだけを書き、値は読まない・書かない。
func CheckSecretLocality(store string, claim core.Scope) error {
	prefix, err := SecretPrefix(claim)
	if err != nil {
		return err
	}
	paths, err := render.NewFileSecretBackend(store).Paths()
	if err != nil {
		return fmt.Errorf("site-local credentials are not available: %w", err)
	}
	var foreign []string
	for _, p := range paths {
		if !strings.HasPrefix(p, prefix) {
			foreign = append(foreign, p)
		}
	}
	if len(foreign) > 0 {
		return fmt.Errorf("secret store %s holds %d credential(s) outside this site's prefix %q (e.g. %q); "+
			"a site keeps only its own devices' credentials (§20.5) — rebuild it with `sakutei secrets extract`",
			store, len(foreign), prefix, foreign[0])
	}
	return nil
}

// ExtractSiteSecrets は中央の file store（src）から claim の拠点の資格だけを抜き出して
// 拠点ローカルの store（dst）を作る（§20.5）。抜き出した件数を返す。
func ExtractSiteSecrets(src, dst string, claim core.Scope) (int, error) {
	prefix, err := SecretPrefix(claim)
	if err != nil {
		return 0, err
	}
	return render.ExtractFileSecretStore(src, dst, func(p string) bool {
		return strings.HasPrefix(p, prefix)
	})
}
