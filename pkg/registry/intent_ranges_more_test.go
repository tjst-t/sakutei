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

func TestHostsRanges(t *testing.T) {
	spec := hostsRanges{setting: "file_hosts"}
	entry := map[string]any{"hosts": []any{"web2", "web1"}}
	got, warnings, err := provider.MergeRanges("host-file", spec, entry, map[string]string{})
	if err != nil || len(warnings) != 0 || got["file_hosts"] != "web1,web2" {
		t.Fatalf("intent only: %v %v %v", got, warnings, err)
	}
	// Config only: a warning, as before.
	got, warnings, err = provider.MergeRanges("host-file", spec, nil, map[string]string{"file_hosts": "web1"})
	if err != nil || len(warnings) != 1 || got["file_hosts"] != "web1" {
		t.Fatalf("config only: %v %v %v", got, warnings, err)
	}
	// The same set in another order agrees; another set disagrees.
	if _, _, err := provider.MergeRanges("host-file", spec, entry, map[string]string{"file_hosts": "web1, web2"}); err != nil {
		t.Fatalf("same hosts: %v", err)
	}
	if _, _, err := provider.MergeRanges("host-file", spec, entry, map[string]string{"file_hosts": "web1"}); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("other hosts: %v", err)
	}
	for name, e := range map[string]map[string]any{
		"twice":   {"hosts": []any{"a", "a"}},
		"empty":   {"hosts": []any{}},
		"comma":   {"hosts": []any{"a,b"}},
		"not str": {"hosts": []any{1}},
		"unknown": {"host": []any{"a"}},
	} {
		if _, _, err := provider.MergeRanges("host-file", spec, e, map[string]string{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGuestosRanges(t *testing.T) {
	entry := map[string]any{"hosts": []any{"node1"}, "markers": map[string]any{"installed": "/etc/sakutei-installed"}}
	got, warnings, err := provider.MergeRanges("guest-os", guestosRanges{}, entry, map[string]string{"guest_markers": "boot=/var/lib/boot"})
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "providers.guest-os.markers.boot") {
		t.Fatalf("%v %v", warnings, err)
	}
	if got["guest_hosts"] != "node1" || got["guest_markers"] != "boot=/var/lib/boot,installed=/etc/sakutei-installed" {
		t.Fatalf("%v", got)
	}
	if _, _, err := provider.MergeRanges("guest-os", guestosRanges{}, entry, map[string]string{"guest_markers": "installed=/etc/other"}); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("a disagreeing marker: %v", err)
	}
	if _, _, err := provider.MergeRanges("guest-os", guestosRanges{}, map[string]any{"markers": map[string]any{"x": "relative"}}, map[string]string{}); err == nil {
		t.Fatal("a relative marker path was accepted")
	}
}

func TestNetconfRanges(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "subtrees.yaml")
	if err := os.WriteFile(list, []byte("subtrees:\n  if-uplink:\n    filter: '<interfaces xmlns=\"urn:x\"><interface><name>uplink</name></interface></interfaces>'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The same filter written with a prefix and other whitespace agrees.
	same := map[string]any{"subtrees": map[string]any{"if-uplink": map[string]any{"filter": "<if:interfaces xmlns:if=\"urn:x\">\n  <if:interface><if:name>uplink</if:name></if:interface>\n</if:interfaces>"}}}
	got, warnings, err := provider.MergeRanges("netconf", netconfRanges{}, same, map[string]string{"netconf_subtrees": list})
	if err != nil || len(warnings) != 0 || got[netconfIntentSubtrees] != "" {
		t.Fatalf("same filter: %v %v %v", got, warnings, err)
	}
	other := map[string]any{"subtrees": map[string]any{"if-uplink": map[string]any{"filter": "<interfaces xmlns=\"urn:x\"/>"}}}
	if _, _, err := provider.MergeRanges("netconf", netconfRanges{}, other, map[string]string{"netconf_subtrees": list}); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("other filter: %v", err)
	}
	// Intent only: the filter as written is passed to the provider.
	only := map[string]any{"hosts": []any{"sw1"}, "subtrees": map[string]any{"acl": map[string]any{"filter": "<acls xmlns=\"urn:y\"/>"}}}
	got, _, err = provider.MergeRanges("netconf", netconfRanges{}, only, map[string]string{"netconf_subtrees": list})
	if err != nil || got["netconf_hosts"] != "sw1" {
		t.Fatalf("intent only: %v %v", got, err)
	}
	subtrees, err := netconfSubtrees(got)
	if err != nil || len(subtrees) != 2 || subtrees["acl"].Filter != "<acls xmlns=\"urn:y\"/>" {
		t.Fatalf("subtrees: %v %v", subtrees, err)
	}
	if _, err := netconfSubtrees(map[string]string{}); err == nil || !strings.Contains(err.Error(), "providers.netconf.subtrees") {
		t.Fatalf("no subtrees: %v", err)
	}
	for name, e := range map[string]map[string]any{
		"filter_file":  {"subtrees": map[string]any{"a": map[string]any{"filter_file": "x.xml"}}},
		"no namespace": {"subtrees": map[string]any{"a": map[string]any{"filter": "<a/>"}}},
		"bad name":     {"subtrees": map[string]any{"A": map[string]any{"filter": "<a xmlns=\"urn:x\"/>"}}},
	} {
		if _, _, err := provider.MergeRanges("netconf", netconfRanges{}, e, map[string]string{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTreeRanges(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ldap.yaml")
	body := "secret_store: s.json\nscope: site=tokyo\nservers:\n  dir1:\n    uri: ldap://x\n    filter: (objectClass=*)\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config := map[string]string{"ldap_config": file}
	entry := map[string]any{
		"scope":   map[string]any{"site": "tokyo"},
		"servers": map[string]any{"dir1": map[string]any{"base_dn": "DC=Example,dc=com", "filter": "(objectClass=*)"}},
	}
	got, warnings, err := provider.MergeRanges("ldap", ldapRanges, entry, config)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("%v %v", warnings, err)
	}
	_, tree, err := ldapRanges.Tree(got)
	if err != nil {
		t.Fatal(err)
	}
	// base_dn (only in the intent) is laid over the file as written.
	if s := tree["servers"].(map[string]any)["dir1"].(map[string]any)["base_dn"]; s != "DC=Example,dc=com" {
		t.Fatalf("base_dn: %v", s)
	}
	// The file's base_dn in another case agrees; another base disagrees.
	if err := os.WriteFile(file, []byte(body+"    base_dn: dc=example,DC=com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.MergeRanges("ldap", ldapRanges, entry, config); err != nil {
		t.Fatalf("same base: %v", err)
	}
	if err := os.WriteFile(file, []byte(body+"    base_dn: dc=example,dc=net\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.MergeRanges("ldap", ldapRanges, entry, config); err == nil || !strings.Contains(err.Error(), "providers.ldap.servers.dir1.base_dn") {
		t.Fatalf("other base: %v", err)
	}
	// Config only warns, naming the intent's key.
	_, warnings, err = provider.MergeRanges("ldap", ldapRanges, nil, config)
	if err != nil || len(warnings) != 3 || !strings.Contains(strings.Join(warnings, "\n"), "providers.ldap.servers.dir1.filter") {
		t.Fatalf("config only: %v %v", warnings, err)
	}
	for name, e := range map[string]map[string]any{
		"unknown server": {"servers": map[string]any{"nosuch": map[string]any{"base_dn": "dc=x"}}},
		"unknown key":    {"servers": map[string]any{"dir1": map[string]any{"uri": "ldap://y"}}},
		"bad filter":     {"servers": map[string]any{"dir1": map[string]any{"filter": "objectClass=*"}}},
		"bad dn":         {"servers": map[string]any{"dir1": map[string]any{"base_dn": "nodn"}}},
		"top unknown":    {"base_dn": "dc=x"},
	} {
		if _, _, err := provider.MergeRanges("ldap", ldapRanges, e, config); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
