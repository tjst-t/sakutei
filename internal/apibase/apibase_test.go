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

package apibase

import "testing"

func TestClean(t *testing.T) {
	ok := map[string]string{
		"/":           "",
		"//":          "",
		"/api2/json":  "/api2/json",
		"/api2/json/": "/api2/json",
		"/mock/v1":    "/mock/v1",
	}
	for in, want := range ok {
		got, err := Clean(in)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "api2/json", "/a//b", "/a/../b", "/a/./b", "/a?x=1", "/a#f", "/a%2F", "/a b", "/a\\b"} {
		if got, err := Clean(in); err == nil {
			t.Errorf("Clean(%q) = %q; want an error", in, got)
		}
	}
}
