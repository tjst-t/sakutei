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
	"strings"
	"testing"
)

func TestParseSecretRef(t *testing.T) {
	for in, want := range map[string]string{
		"secretref:file/db/root#gen3":                    "secretref:file/db/root#gen3",
		" secretref:file/app/token ":                     "secretref:file/app/token#gen0",
		`{"$secret":"db/root","backend":"file","gen":2}`: "secretref:file/db/root#gen2",
	} {
		r, err := ParseSecretRef(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseSecretRef(%q) = %v, %v; want %s", in, r, err, want)
		}
	}
	for _, in := range []string{"hunter2", "secretref:file", "secretref:file/x#genX", "{}"} {
		_, err := ParseSecretRef(in)
		if err == nil {
			t.Errorf("ParseSecretRef(%q): want an error", in)
		} else if in == "hunter2" && strings.Contains(err.Error(), in) {
			t.Errorf("the error quotes the input: %v", err)
		}
	}
}
