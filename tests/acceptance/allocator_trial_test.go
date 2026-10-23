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

// allocator_trial_test.go は Backlog #98（採番の試用、docs/trial/allocation）で見つけた
// allocator HA 最小形（§19.8）の不具合の受け入れテスト。
//
// AC tags:
//
//	[AC-98-3]  --standby なしの `sakutei allocator` が listen に失敗したら、昇格しない
//	           （世代を上げない）。以前は昇格してから listen していたので、使用中のポートで
//	           起動を誤ると今の active が fence され、active が 1 つも無くなった
//	[AC-98-4]  新しい世代に fence された active は、GET /v1/health でも standby と答える
//	           （以前は次の操作が来るまで active と答え、2 つの active が見えた）
package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/allocator"
)

func TestAllocatorTrial_FailedStartDoesNotPromote(t *testing.T) {
	// [AC-98-3]
	_, store := newAuthority(t, 9000, 9009)
	active := startAllocator(t, store, "127.0.0.1:0")
	addr := strings.TrimPrefix(active.url, "http://")

	out, code := runSakutei(t, "allocator", "--store", store, "--listen", addr)
	if code == 0 || !strings.Contains(out, "listen") {
		t.Fatalf("[AC-98-3] a second allocator on a port in use must fail to listen (exit %d):\n%s", code, out)
	}
	if got := readFile(t, store); !strings.Contains(got, `"generation": 1`) {
		t.Fatalf("[AC-98-3] the failed start must not raise the fencing generation:\n%s", got)
	}
	v, err := allocator.NewClient([]string{active.url}).Allocate(context.Background(), "p", "net-1")
	if err != nil || v != "9000" {
		t.Fatalf("[AC-98-3] the running active must still allocate: %q %v", v, err)
	}
}

func TestAllocatorTrial_HealthOfFencedActive(t *testing.T) {
	// [AC-98-4]
	_, path := newAuthority(t, 100, 199)
	srvA := allocator.NewServer(allocator.New(path))
	if _, err := srvA.Promote(); err != nil {
		t.Fatal(err)
	}
	srvB := allocator.NewServer(allocator.New(path))
	a := httptest.NewServer(srvA)
	defer a.Close()
	health := func(url string) map[string]any {
		t.Helper()
		res, err := http.Get(url + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if r := health(a.URL)["role"]; r != "active" {
		t.Fatalf("[AC-98-4] A must report active before the failover, got %v", r)
	}
	if _, err := srvB.Promote(); err != nil {
		t.Fatal(err)
	}
	if r := health(a.URL)["role"]; r != "standby" {
		t.Fatalf("[AC-98-4] A fenced by B's promotion must report standby in health, got %v", r)
	}
	if srvA.Active() {
		t.Fatalf("[AC-98-4] the health check must make the fenced A step down")
	}
}
