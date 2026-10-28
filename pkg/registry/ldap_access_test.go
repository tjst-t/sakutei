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

package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// TestLDAPFactoryAccess: the ldap factory reads the run's access (§20.7,
// Backlog #186): an observe-only run starts without the write bind password,
// a run that may write does not, and an unknown access is refused.
func TestLDAPFactoryAccess(t *testing.T) {
	// [AC-186-4-4]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.json"), []byte(`{"secrets": {"ldap/read": {"value": "r"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "ldap.yaml")
	body := "secret_store: s.json\nservers:\n  dir1:\n    uri: ldap://x\n    base_dn: dc=example,dc=com\n" +
		"    observe:\n      bind_dn: cn=read\n      bind_password: secretref:file/ldap/read\n" +
		"    write:\n      bind_dn: cn=write\n      bind_password: secretref:file/ldap/write\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := DefaultRegistry()
	if _, err := r.Get("ldap", map[string]string{"ldap_config": file, provider.ConfigAccess: provider.AccessObserve}); err != nil {
		t.Fatalf("an observe-only run: %v", err)
	}
	if _, err := r.Get("ldap", map[string]string{"ldap_config": file}); err == nil || !strings.Contains(err.Error(), "servers.dir1.write.bind_password") {
		t.Fatalf("a writing run without the write password: %v", err)
	}
	if _, err := r.Get("ldap", map[string]string{"ldap_config": file, provider.ConfigAccess: "read"}); err == nil {
		t.Fatal("an unknown access must be refused")
	}
}
