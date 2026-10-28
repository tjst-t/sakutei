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

// provider_intent_rest_test.go — Backlog #182: the second half of A3.  The
// range settings of the rest of the built-in providers are written in the
// intent's providers: section (§7.7), by the common rule of #181: only in the
// provider config is read as before with a warning, only in the intent is
// taken, in both must agree or the run is refused at load time.
//
// AC tags:
//
//	AC-182-1  host-file: providers.host-file.hosts alone manages a local file
//	          (no file_hosts in the config, no warning); file_hosts only in
//	          the config warns; the same set in both is quiet; a different set
//	          is refused by plan, reconcile and status (the file untouched)
//	AC-182-2  ldap: the search base and filter of a server
//	          (providers.ldap.servers.<name>.base_dn / filter) written only in
//	          the intent — the server list has neither — take effect (an entry
//	          outside the filter is not pruned); base_dn only in the server
//	          list warns; a different base is refused; an unknown server is
//	          refused
//	AC-182-3  every range of the list (docs/providers/ranges.md) can be
//	          written in the intent: netconf hosts / subtrees, guest-os hosts /
//	          markers, systemd-unit hosts, cli-device hosts, postgresql scope,
//	          s3-bucket scope / bucket_prefix — each builds from the intent
//	          alone without a warning, warns when only in the config, and is
//	          refused when the two disagree
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/clidevice"
	"github.com/tjst-t/sakutei/v2/internal/guestos"
	"github.com/tjst-t/sakutei/v2/internal/hostconf"
	"github.com/tjst-t/sakutei/v2/internal/ldap"
	"github.com/tjst-t/sakutei/v2/internal/ldap/ldaptest"
	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/postgresql"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

func TestProviderIntent_HostFileHosts(t *testing.T) {
	// [AC-182-1]
	bin := buildBinary(t)
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "etc", "app.conf")
	writeIntent := func(hosts []any) {
		in := map[string]any{"resources": []map[string]any{
			{"provider": hostconf.FileName, "id": "local" + target, "fields": map[string]string{"content": "listen 8080\n"}},
		}}
		if hosts != nil {
			in["providers"] = map[string]any{hostconf.FileName: map[string]any{"hosts": hosts}}
		}
		writeIntentFile(t, intentDir, in)
	}
	run := func(verb, fileHosts string) (string, int) {
		t.Helper()
		pc := map[string]any{"stamp_dir": filepath.Join(tmp, "stamps")}
		if fileHosts != "" {
			pc["file_hosts"] = fileHosts
		}
		path := filepath.Join(tmp, "providers.json")
		writeJSONFile(t, path, map[string]any{"providers": map[string]any{hostconf.FileName: pc}})
		return runBin(t, bin, verb, "--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", path)
	}

	// Only in the intent: the file is written, without a warning.
	writeIntent([]any{"local"})
	if out, code := run("reconcile", ""); code != 0 || strings.Contains(out, provIntentWarn) {
		t.Fatalf("[AC-182-1] hosts only in the intent: exit %d, want 0 without a warning:\n%s", code, out)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "listen 8080\n" {
		t.Fatalf("[AC-182-1] hosts only in the intent: the file was not written: %q %v", b, err)
	}
	// The same in both: quiet.
	if out, code := run("plan", "local"); code != 0 || strings.Contains(out, provIntentWarn) || strings.Contains(out, "disagree") {
		t.Fatalf("[AC-182-1] the same hosts in both: exit %d:\n%s", code, out)
	}
	// Only in the config: a warning naming the intent's key, and it works.
	writeIntent(nil)
	if err := os.WriteFile(target, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run("reconcile", "local")
	if code != 0 || !strings.Contains(out, provIntentWarn) || !strings.Contains(out, "providers.host-file.hosts") {
		t.Fatalf("[AC-182-1] file_hosts only in the config: exit %d, want a warning naming providers.host-file.hosts:\n%s", code, out)
	}
	if b, _ := os.ReadFile(target); string(b) != "listen 8080\n" {
		t.Fatalf("[AC-182-1] file_hosts only in the config no longer works: %q", b)
	}
	// A different set: refused, the file untouched.
	writeIntent([]any{"local"})
	if err := os.WriteFile(target, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"plan", "reconcile", "status"} {
		out, code := run(verb, "local,web1")
		if code == 0 || !strings.Contains(out, "disagree") || !strings.Contains(out, "providers.host-file.hosts") {
			t.Errorf("[AC-182-1] %s with disagreeing hosts: exit %d, want a refusal:\n%s", verb, code, out)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "changed\n" {
		t.Fatalf("[AC-182-1] a refused run changed the file: %q", b)
	}
}

func TestProviderIntent_LDAPBaseAndFilter(t *testing.T) {
	// [AC-182-2]
	lab := newLDAPLab(t)
	// The server list without base_dn: the range is only in the intent.
	body, err := os.ReadFile(lab.config)
	if err != nil {
		t.Fatal(err)
	}
	noBase := strings.Replace(string(body), "    base_dn: dc=example,dc=com\n", "", 1)
	if err := os.WriteFile(lab.config, []byte(noBase), 0o600); err != nil {
		t.Fatal(err)
	}
	ou := func(name string) map[string][]string {
		return map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {name}}
	}
	lab.edit(t, func(st *ldaptest.State) {
		st.Put("ou=leftover,dc=example,dc=com", ou("leftover"))
		st.Put("ou=outside,dc=example,dc=com", ou("outside"))
	})
	intentDir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntent := func(ranges string) {
		in := `resources:
  - provider: ldap
    id: dir1/ou=people,dc=example,dc=com
    deletion_policy: PruneWithinScope
    fields:
      objectClass: '["top","organizationalUnit"]'
      ou: People
` + ranges
		if err := os.WriteFile(filepath.Join(intentDir, "ldap.yaml"), []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ranges := func(base string) string {
		return "providers:\n  ldap:\n    servers:\n      dir1:\n        base_dn: " + base + "\n        filter: (!(ou=outside))\n"
	}
	bin := buildBinary(t)
	run := func(verb string) (string, int) {
		return runBin(t, bin, verb, "--intent", intentDir, "--state", filepath.Join(lab.dir, "fake.json"), "--provider-config", lab.providerConfig)
	}

	writeIntent(ranges("DC=Example,dc=com"))
	if out, code := run("reconcile"); code != 0 || strings.Contains(out, provIntentWarn) {
		t.Fatalf("[AC-182-2] base_dn and filter only in the intent: exit %d, want 0 without a warning:\n%s", code, out)
	}
	st := lab.edit(t, func(*ldaptest.State) {})
	if st.Find("ou=people,dc=example,dc=com") == nil {
		t.Fatal("[AC-182-2] the declared entry was not created")
	}
	if st.Find("ou=leftover,dc=example,dc=com") != nil {
		t.Error("[AC-182-2] the orphan under the intent's base was not pruned")
	}
	if st.Find("ou=outside,dc=example,dc=com") == nil {
		t.Error("[AC-182-2] the entry outside the intent's filter was pruned (the filter did not take effect)")
	}

	// base_dn only in the server list: a warning naming the intent's key.
	if err := os.WriteFile(lab.config, body, 0o600); err != nil {
		t.Fatal(err)
	}
	writeIntent("")
	if out, code := run("plan"); code != 0 || !strings.Contains(out, provIntentWarn) || !strings.Contains(out, "providers.ldap.servers.dir1.base_dn") {
		t.Fatalf("[AC-182-2] base_dn only in the server list: exit %d, want a warning:\n%s", code, out)
	}
	// The same base (in another case) in both: no disagreement.
	writeIntent(ranges("dc=EXAMPLE,dc=com"))
	if out, code := run("plan"); code != 0 || strings.Contains(out, "disagree") {
		t.Fatalf("[AC-182-2] the same base in both: exit %d:\n%s", code, out)
	}
	// A different base, and an unknown server: refused.
	writeIntent(ranges("ou=people,dc=example,dc=com"))
	for _, verb := range []string{"plan", "reconcile", "status"} {
		if out, code := run(verb); code == 0 || !strings.Contains(out, "disagree") || !strings.Contains(out, "providers.ldap.servers.dir1.base_dn") {
			t.Errorf("[AC-182-2] %s with a disagreeing base: exit %d, want a refusal:\n%s", verb, code, out)
		}
	}
	writeIntent("providers:\n  ldap:\n    servers:\n      dir9:\n        filter: (objectClass=*)\n")
	if out, code := run("plan"); code == 0 || !strings.Contains(out, "no such server") {
		t.Errorf("[AC-182-2] an unknown server: exit %d, want a refusal:\n%s", code, out)
	}
}

func TestProviderIntent_RestOfTheList(t *testing.T) {
	// [AC-182-3]
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("known_hosts", "")
	hosts := write("hosts.yaml", "ssh: /bin/true\nknown_hosts: known_hosts\nhosts:\n  sw1:\n    address: 192.0.2.1\n    port: 830\n  node1:\n    address: 192.0.2.2\n  dev1:\n    address: 192.0.2.3\n")
	subtrees := write("subtrees.yaml", "subtrees:\n  if-uplink:\n    filter: '"+ncFilter+"'\n")
	def := write("tiny.yaml", cdExecDef)
	store := `{"secrets": {"s3/fake/access_key": {"gen": 1, "value": "a"}, "s3/fake/secret_key": {"gen": 1, "value": "b"}}}`
	write("secrets.json", store)
	pg := func(extra string) string {
		return write("pg.yaml", "psql: /nonexistent/psql\n"+extra+"servers:\n  main:\n    host: db.test\n    user: sakutei\n")
	}
	s3 := func(extra string) string {
		return write("s3.yaml", "secret_store: secrets.json\nendpoints:\n  fake:\n    endpoint: http://127.0.0.1:1\n"+
			"    access_key: secretref:file/s3/fake/access_key\n    secret_key: secretref:file/s3/fake/secret_key\n"+extra)
	}
	tokyo := map[string]any{"site": "tokyo"}

	type range_ struct {
		key    string            // the intent's key, as the warning names it
		entry  map[string]any    // the intent's providers.<name>: entry
		config map[string]string // the config with the same range
		other  map[string]string // the config with a different range
		base   map[string]string // the config without it (the connection only)
		// file rewrites a provider's own file before the config is used
		// (a range held in the file); nil: none.
		file func(which string)
	}
	cases := map[string][]range_{
		netconf.Name: {
			{
				key:    "providers.netconf.hosts",
				entry:  map[string]any{"hosts": []any{"sw1"}},
				config: map[string]string{"remote_config": hosts, "netconf_subtrees": subtrees, "netconf_hosts": "sw1"},
				other:  map[string]string{"remote_config": hosts, "netconf_subtrees": subtrees, "netconf_hosts": "sw1,node1"},
				base:   map[string]string{"remote_config": hosts, "netconf_subtrees": subtrees},
			},
			{
				key:    "providers.netconf.subtrees.if-uplink",
				entry:  map[string]any{"subtrees": map[string]any{"if-uplink": map[string]any{"filter": ncFilter}}},
				config: map[string]string{"remote_config": hosts, "netconf_hosts": "sw1", "netconf_subtrees": subtrees},
				other: map[string]string{"remote_config": hosts, "netconf_hosts": "sw1", "netconf_subtrees": write("other-subtrees.yaml",
					"subtrees:\n  if-uplink:\n    filter: '<interfaces xmlns=\"urn:other\"/>'\n")},
				base: map[string]string{"remote_config": hosts, "netconf_hosts": "sw1"},
			},
		},
		guestos.Name: {
			{
				key:    "providers.guest-os.hosts",
				entry:  map[string]any{"hosts": []any{"node1"}},
				config: map[string]string{"remote_config": hosts, "guest_hosts": "node1"},
				other:  map[string]string{"remote_config": hosts, "guest_hosts": "node1,sw1"},
				base:   map[string]string{"remote_config": hosts},
			},
			{
				key:    "providers.guest-os.markers.installed",
				entry:  map[string]any{"markers": map[string]any{"installed": "/etc/sakutei-installed"}},
				config: map[string]string{"remote_config": hosts, "guest_hosts": "node1", "guest_markers": "installed=/etc/sakutei-installed"},
				other:  map[string]string{"remote_config": hosts, "guest_hosts": "node1", "guest_markers": "installed=/etc/other"},
				base:   map[string]string{"remote_config": hosts, "guest_hosts": "node1"},
			},
		},
		hostconf.UnitName: {{
			key:    "providers.systemd-unit.hosts",
			entry:  map[string]any{"hosts": []any{"local"}},
			config: map[string]string{"unit_hosts": "local"},
			other:  map[string]string{"unit_hosts": "local,node1", "remote_config": hosts},
			base:   map[string]string{},
		}},
		clidevice.Name: {{
			key:    "providers.cli-device.hosts",
			entry:  map[string]any{"hosts": []any{"dev1"}},
			config: map[string]string{"remote_config": hosts, "cli_definition": def, "cli_hosts": "dev1"},
			other:  map[string]string{"remote_config": hosts, "cli_definition": def, "cli_hosts": "dev1,sw1"},
			base:   map[string]string{"remote_config": hosts, "cli_definition": def},
		}},
		postgresql.Name: {
			{
				key:   "providers.postgresql.scope",
				entry: map[string]any{"scope": tokyo},
				file: func(which string) {
					pg(map[string]string{"same": "scope: site=tokyo\n", "other": "scope: site=osaka\n", "base": ""}[which])
				},
			},
			{
				key:   "providers.postgresql.servers.main.scope",
				entry: map[string]any{"servers": map[string]any{"main": map[string]any{"scope": tokyo}}},
				file: func(which string) {
					v := map[string]string{"same": "site=tokyo", "other": "site=osaka"}[which]
					body := "psql: /nonexistent/psql\nservers:\n  main:\n    host: db.test\n    user: sakutei\n"
					if v != "" {
						body += "    scope: " + v + "\n"
					}
					write("pg.yaml", body)
				},
			},
		},
		s3bucket.Name: {
			{
				key:   "providers.s3-bucket.scope",
				entry: map[string]any{"scope": tokyo},
				file: func(which string) {
					v := map[string]string{"same": "site=tokyo", "other": "site=osaka"}[which]
					if v == "" {
						s3("")
						return
					}
					write("s3.yaml", "scope: "+v+"\n"+readFileString(s3("")))
				},
			},
			{
				key:   "providers.s3-bucket.endpoints.fake.bucket_prefix",
				entry: map[string]any{"endpoints": map[string]any{"fake": map[string]any{"bucket_prefix": "app-"}}},
				file: func(which string) {
					s3(map[string]string{"same": "    bucket_prefix: app-\n", "other": "    bucket_prefix: db-\n", "base": ""}[which])
				},
			},
		},
	}
	fileConfig := map[string]map[string]string{
		postgresql.Name: {"postgresql_config": filepath.Join(dir, "pg.yaml")},
		s3bucket.Name:   {"s3_config": filepath.Join(dir, "s3.yaml")},
	}
	reg := registry.DefaultRegistry()
	for name, rs := range cases {
		for _, r := range rs {
			config := func(which string) map[string]string {
				if r.file == nil {
					return map[string]map[string]string{"same": r.config, "other": r.other, "base": r.base}[which]
				}
				r.file(which)
				return fileConfig[name]
			}
			// Only in the intent: built, no warning about it.
			p, warnings, err := reg.GetWithIntent(name, config("base"), r.entry)
			if err != nil || strings.Contains(strings.Join(warnings, "\n"), r.key) {
				t.Errorf("[AC-182-3] %s only in the intent: err %v, warnings %v", r.key, err, warnings)
			} else if err := sdk.ValidateProvider(p); err != nil {
				t.Errorf("[AC-182-3] %s only in the intent: %v", r.key, err)
			}
			// Only in the config: built as before, with a warning naming the
			// intent's key.
			_, warnings, err = reg.GetWithIntent(name, config("same"), nil)
			if err != nil || !strings.Contains(strings.Join(warnings, "\n"), r.key) {
				t.Errorf("[AC-182-3] %s only in the config: err %v, want a warning naming it: %v", r.key, err, warnings)
			}
			// Both the same: no warning about it.
			_, warnings, err = reg.GetWithIntent(name, config("same"), r.entry)
			if err != nil || strings.Contains(strings.Join(warnings, "\n"), r.key) {
				t.Errorf("[AC-182-3] %s the same in both: err %v, warnings %v", r.key, err, warnings)
			}
			// A disagreement: refused.
			if _, _, err := reg.GetWithIntent(name, config("other"), r.entry); err == nil || !strings.Contains(err.Error(), "disagree") || !strings.Contains(err.Error(), r.key) {
				t.Errorf("[AC-182-3] %s disagreeing: err %v, want a refusal naming it", r.key, err)
			}
		}
	}
	// The ldap ranges (AC-182-2) and the ones of #181 are not left out of the
	// list: every provider of the list takes its ranges from the intent.
	for _, name := range []string{netconf.Name, guestos.Name, hostconf.FileName, hostconf.UnitName, clidevice.Name, ldap.Name, postgresql.Name, s3bucket.Name, "dnszone", "nftables", "proxmox-vm"} {
		if _, ok := reg.IntentRangesOf(name); !ok {
			t.Errorf("[AC-182-3] %s takes no range settings from the intent", name)
		}
	}
}
