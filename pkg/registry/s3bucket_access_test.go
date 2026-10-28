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

// TestS3BucketFactoryAccess: the s3-bucket factory reads the run's access
// (§20.7, Backlog #186): an observe-only run starts without the write keys,
// a run that may write does not, and an unknown access is refused.
func TestS3BucketFactoryAccess(t *testing.T) {
	// [AC-186-6-6]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.json"), []byte(`{"secrets": {"s3/ro_ak": {"value": "a"}, "s3/ro_sk": {"value": "s"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "s3.yaml")
	body := "secret_store: s.json\nendpoints:\n  minio:\n    endpoint: http://127.0.0.1:9000\n" +
		"    observe:\n      access_key: secretref:file/s3/ro_ak\n      secret_key: secretref:file/s3/ro_sk\n" +
		"    write:\n      access_key: secretref:file/s3/rw_ak\n      secret_key: secretref:file/s3/rw_sk\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := DefaultRegistry()
	if _, err := r.Get("s3-bucket", map[string]string{"s3_config": file, provider.ConfigAccess: provider.AccessObserve}); err != nil {
		t.Fatalf("an observe-only run: %v", err)
	}
	if _, err := r.Get("s3-bucket", map[string]string{"s3_config": file}); err == nil || !strings.Contains(err.Error(), "endpoints.minio.write.access_key") {
		t.Fatalf("a writing run without the write keys: %v", err)
	}
	if _, err := r.Get("s3-bucket", map[string]string{"s3_config": file, provider.ConfigAccess: "read"}); err == nil {
		t.Fatal("an unknown access must be refused")
	}
}
