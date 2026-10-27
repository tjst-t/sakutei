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

package s3bucket

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const testStore = `{"secrets": {"s3/ak": {"gen": 1, "value": "AKIA-TEST"}, "s3/sk": {"gen": 1, "value": "very-secret-key"}}}`

func TestLoadConfig(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"secrets.json": testStore,
		"s3.yaml": `secret_store: secrets.json
scope: site=tokyo
endpoints:
  minio:
    endpoint: http://127.0.0.1:9000
    access_key: secretref:file/s3/ak
    secret_key:
      $secret: s3/sk
      backend: file
    bucket_prefix: app-
    timeout: 3s
  aws:
    endpoint: https://s3.ap-northeast-1.amazonaws.com
    region: ap-northeast-1
    addressing: virtual
    access_key: secretref:file/s3/ak
    secret_key: secretref:file/s3/sk
`,
	})
	cfg, err := LoadConfig(filepath.Join(dir, "s3.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Endpoints["minio"]
	if m.Region != DefaultRegion || m.Addressing != AddressingPath || m.BucketPrefix != "app-" || m.Timeout.String() != "3s" || m.SecretKey.Path() != "s3/sk" {
		t.Errorf("minio = %+v", m)
	}
	if a := cfg.Endpoints["aws"]; a.Addressing != AddressingVirtual || a.Region != "ap-northeast-1" {
		t.Errorf("aws = %+v", a)
	}
	if cfg.Scope.Labels["site"] != "tokyo" || cfg.SecretStore != filepath.Join(dir, "secrets.json") {
		t.Errorf("cfg = %+v", cfg)
	}
	p, err := NewFromConfig(filepath.Join(dir, "s3.yaml"))
	if err != nil || len(p.endpoints) != 2 {
		t.Fatalf("NewFromConfig = %v, %v", p, err)
	}
}

func TestLoadConfig_Errors(t *testing.T) {
	base := func(ep string) string {
		return "secret_store: secrets.json\nendpoints:\n  e1:\n" + ep
	}
	ok := "    endpoint: http://h:9000\n    access_key: secretref:file/s3/ak\n    secret_key: secretref:file/s3/sk\n"
	for _, tc := range []struct {
		yaml, want string
	}{
		{base(ok + "    colour: red\n"), `unknown key "endpoints.e1.colour"`},
		{base("    endpoint: http://h\n    access_key: AKIA-PLAIN\n    secret_key: secretref:file/s3/sk\n"), "must be a SecretRef"},
		{base("    endpoint: http://h\n    access_key: secretref:file/s3/ak\n    secret_key: plain-secret-value\n"), "must be a SecretRef"},
		{base("    endpoint: http://h\n    access_key: secretref:file/s3/ak\n    secret_key: secretref:file/s3/missing\n"), "is not in the secret store"},
		{base("    endpoint: http://h\n    access_key: secretref:vault/s3/ak\n    secret_key: secretref:file/s3/sk\n"), "only the file SecretBackend"},
		{base(ok + "    addressing: dns\n"), "want path or virtual"},
		{base(ok + "    insecure_skip_verify: yes-please\n"), "want true or false"},
		{base(ok + "    timeout: soon\n"), "positive duration"},
		{base("    access_key: secretref:file/s3/ak\n    secret_key: secretref:file/s3/sk\n"), "endpoint is required"},
		{"endpoints:\n  e1:\n" + ok, "secret_store"},
		{"secret_store: secrets.json\nendpoints:\n  Bad/Name:\n" + ok, "the name must match"},
		{base(ok + "    fields: versioning\n"), "fields: want a list"},
		{base(ok + "    fields:\n      - versioning\n      - acl\n"), "fields[1] acl: want one of"},
		{base(ok + "    fields:\n      - tags\n      - tags\n"), `duplicate "tags"`},
		{base(ok + "    storage_class_aliases: GLACIER\n"), "storage_class_aliases: want a map"},
		{base(ok + "    storage_class_aliases:\n      COLD: \"\"\n"), "non-empty storage class name"},
	} {
		dir := writeFiles(t, map[string]string{"secrets.json": testStore, "s3.yaml": tc.yaml})
		_, err := LoadConfig(filepath.Join(dir, "s3.yaml"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoadConfig(%q) = %v; want %q", tc.yaml, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "plain-secret-value") || strings.Contains(err.Error(), "AKIA-PLAIN")) {
			t.Errorf("the error shows a key: %v", err)
		}
	}
}

func TestLoadConfig_FieldsAndAliases(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"secrets.json": testStore,
		"s3.yaml": `secret_store: secrets.json
endpoints:
  odd:
    endpoint: http://127.0.0.1:9000
    access_key: secretref:file/s3/ak
    secret_key: secretref:file/s3/sk
    fields:
      - policy
      - lifecycle
    storage_class_aliases:
      COLD_TIER: GLACIER
  plain:
    endpoint: http://127.0.0.1:9001
    access_key: secretref:file/s3/ak
    secret_key: secretref:file/s3/sk
`,
	})
	cfg, err := LoadConfig(filepath.Join(dir, "s3.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.Endpoints["odd"]
	if strings.Join(o.Fields, ",") != "policy,lifecycle" || o.StorageClassAliases["COLD_TIER"] != "GLACIER" {
		t.Errorf("odd = %+v", o)
	}
	if pl := cfg.Endpoints["plain"]; pl.Fields != nil || pl.StorageClassAliases != nil {
		t.Errorf("plain = %+v; want the defaults (all fields, no aliases)", pl)
	}
	eps, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	if e := eps["odd"]; e.handles(FieldVersioning) || !e.handles(FieldLifecycle) || e.StorageClassAliases["COLD_TIER"] != "GLACIER" {
		t.Errorf("Build odd = %+v", e)
	}
	if e := eps["plain"]; !e.handles(FieldVersioning) || !e.handles(FieldTags) {
		t.Errorf("Build plain = %+v; want all fields", e)
	}
}
