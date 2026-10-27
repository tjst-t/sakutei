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

package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ParseSecretRef reads the value of a secret-flagged field as a provider's
// Apply receives it (§20, #119): the ref form "secretref:<backend>/<path>"
// with an optional "#gen<N>", or the JSON form
// {"$secret":"<path>","backend":"<backend>","gen":N}.  Anything else — above
// all a bare plaintext — is an error.  The error never quotes s.
func ParseSecretRef(s string) (SecretRef, error) {
	t := strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(t, "secretref:"); ok {
		var gen int64
		if i := strings.LastIndex(rest, "#gen"); i >= 0 {
			n, err := strconv.ParseInt(rest[i+len("#gen"):], 10, 64)
			if err != nil {
				return SecretRef{}, fmt.Errorf("secretref: bad generation")
			}
			gen, rest = n, rest[:i]
		}
		backend, path, ok := strings.Cut(rest, "/")
		if !ok || backend == "" || path == "" {
			return SecretRef{}, fmt.Errorf("secretref: want secretref:<backend>/<path>[#gen<N>]")
		}
		return NewSecretRef(backend, path, gen), nil
	}
	if strings.HasPrefix(t, "{") {
		var r SecretRef
		if err := json.Unmarshal([]byte(t), &r); err == nil && r.Path() != "" {
			return r, nil
		}
	}
	return SecretRef{}, fmt.Errorf("not a SecretRef (want secretref:<backend>/<path>[#gen<N>] or {\"$secret\": ...})")
}
