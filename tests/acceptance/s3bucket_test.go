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

// s3bucket_test.go — Backlog #122: the s3-bucket provider (S3-compatible
// bucket) against the fake S3 server (internal/s3bucket/s3test, which checks
// the SigV4 signature of every request).  Real servers are exercised by
// tests/integration/s3 (make integration-s3).
//
// AC tags:
//
//	AC-122-1  DefaultRegistry builds s3-bucket from s3_config (the endpoint
//	          list; keys are SecretRefs resolved from the file SecretBackend);
//	          without s3_config it is an error; the provider passes
//	          sdk.ContractTester (incl. unreachable → unknown, never absent)
//	AC-122-2  prune under PruneWithinScope: an empty orphan bucket is a
//	          destructive Delete and goes; a bucket with objects is never
//	          deleted (BucketNotEmpty is a PermanentError) and stays
//	AC-122-3  an unreachable endpoint halts prune (List is not complete) and
//	          the declared bucket is unobservable (no op)
//	AC-122-4  sakutei status: a declared field whose API the endpoint does not
//	          implement (NotImplemented) makes that bucket unobservable, with
//	          the reason, and the report goes on for the other buckets
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/s3bucket"
	"github.com/tjst-t/sakutei/internal/s3bucket/s3test"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// writeS3Config writes an endpoint list ("fake" → srv) and its secret store,
// and returns the path of the list.
func writeS3Config(t *testing.T, srv *s3test.Server) string {
	t.Helper()
	dir := t.TempDir()
	store := `{"secrets": {"s3/fake/access_key": {"gen": 1, "value": "test-access"}, "s3/fake/secret_key": {"gen": 1, "value": "test-secret"}}}`
	cfg := "secret_store: secrets.json\nendpoints:\n  fake:\n    endpoint: " + srv.URL + "\n" +
		"    access_key: secretref:file/s3/fake/access_key\n    secret_key: secretref:file/s3/fake/secret_key\n    timeout: 2s\n"
	for name, body := range map[string]string{"secrets.json": store, "s3.yaml": cfg} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "s3.yaml")
}

// s3ProviderFromRegistry builds the provider for srv through DefaultRegistry,
// as the CLI does.
func s3ProviderFromRegistry(t *testing.T, srv *s3test.Server) provider.Provider {
	t.Helper()
	p, err := registry.DefaultRegistry().Get(s3bucket.Name, map[string]string{"s3_config": writeS3Config(t, srv)})
	if err != nil {
		t.Fatalf("DefaultRegistry().Get(s3-bucket): %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	return p
}

func TestS3Bucket_RegistryAndContract(t *testing.T) {
	// [AC-122-1]
	if _, err := registry.DefaultRegistry().Get(s3bucket.Name, map[string]string{"state": "x.json"}); err == nil || !strings.Contains(err.Error(), "--s3-config") {
		t.Errorf("[AC-122-1] s3-bucket without s3_config: err = %v; want one naming --s3-config", err)
	}
	srv := s3test.New(s3test.Options{})
	defer srv.Close()
	p := s3ProviderFromRegistry(t, srv)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p.(provider.Lister),
		Scope:    core.NewScope(nil),
		SampleID: "fake/ac-122-contract",
		SampleDesired: map[string]string{
			s3bucket.FieldVersioning: "Suspended",
			s3bucket.FieldTags:       `{"owner":"sakutei"}`,
		},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
}

func TestS3Bucket_PruneKeepsNonEmptyBuckets(t *testing.T) {
	// [AC-122-2]
	ctx := context.Background()
	srv := s3test.New(s3test.Options{})
	defer srv.Close()
	p := s3ProviderFromRegistry(t, srv)
	for _, b := range []string{"keep-me", "orphan-empty", "orphan-full"} {
		srv.CreateBucket(b)
	}
	srv.PutObject("orphan-full")
	desired := []diff.DesiredResource{{
		ID:             core.NewID(s3bucket.Name, core.NewScope(nil), "fake/keep-me"),
		Fields:         map[string]string{s3bucket.FieldVersioning: "Enabled"},
		DeletionPolicy: core.PruneWithinScope,
	}}
	if err := prune.ValidatePrunePolicy(desired, p); err != nil {
		t.Fatalf("[AC-122-2] ValidatePrunePolicy: %v", err)
	}
	res, err := prune.New().ComputePrune(ctx, p.(provider.Lister), p, core.NewScope(nil), core.NewScope(nil), desired, nil, io.Discard)
	if err != nil || res.Halted {
		t.Fatalf("[AC-122-2] ComputePrune = %+v, %v", res, err)
	}
	if len(res.DeleteOps) != 2 {
		t.Fatalf("[AC-122-2] DeleteOps = %v; want the two orphans", res.DeleteOps)
	}
	for _, op := range res.DeleteOps {
		if !op.Destructive {
			t.Errorf("[AC-122-2] Delete of %s is not destructive (it must count against the budget)", op.Target)
		}
		err := p.Apply(ctx, core.Plan{Ops: []core.Op{op}})
		switch op.Target.Local {
		case "fake/orphan-empty":
			if err != nil {
				t.Errorf("[AC-122-2] Delete of the empty orphan: %v", err)
			}
		case "fake/orphan-full":
			var pe *provider.PermanentError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), "BucketNotEmpty") {
				t.Errorf("[AC-122-2] Delete of the non-empty orphan: %v; want a PermanentError with BucketNotEmpty", err)
			}
		}
	}
	if srv.Has("orphan-empty") || !srv.Has("orphan-full") || !srv.Has("keep-me") {
		t.Errorf("[AC-122-2] after prune: orphan-empty=%v orphan-full=%v keep-me=%v; want false true true",
			srv.Has("orphan-empty"), srv.Has("orphan-full"), srv.Has("keep-me"))
	}
}

func TestS3Bucket_UnreachableDoesNothing(t *testing.T) {
	// [AC-122-3]
	ctx := context.Background()
	srv := s3test.New(s3test.Options{})
	defer srv.Close()
	p := s3ProviderFromRegistry(t, srv)
	srv.CreateBucket("orphan-empty")
	srv.SetUnreachable(true)
	id := core.NewID(s3bucket.Name, core.NewScope(nil), "fake/declared")
	desired := []diff.DesiredResource{{ID: id, Fields: map[string]string{s3bucket.FieldVersioning: "Enabled"}, DeletionPolicy: core.PruneWithinScope}}

	obs, err := p.Read(ctx, id.Local)
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("[AC-122-3] Read = %+v, %v; want Unknown", obs, err)
	}
	if pl := diff.ForProvider(p).Diff(obs, desired[0], cause.CauseContext{}); len(pl.Ops) != 0 {
		t.Errorf("[AC-122-3] plan while unreachable = %v; want nothing", pl.Ops)
	}
	res, err := prune.New().ComputePrune(ctx, p.(provider.Lister), p, core.NewScope(nil), core.NewScope(nil), desired, nil, io.Discard)
	if err != nil || !res.Halted || len(res.DeleteOps) != 0 {
		t.Errorf("[AC-122-3] ComputePrune while unreachable = %+v, %v; want halted with no Delete", res, err)
	}
	srv.SetUnreachable(false)
	if !srv.Has("orphan-empty") {
		t.Error("[AC-122-3] a bucket was deleted while the endpoint was unreachable")
	}
}

func TestS3Bucket_StatusReportsAnUnimplementedAPIAsUnobservable(t *testing.T) {
	// [AC-122-4]
	srv := s3test.New(s3test.Options{NotImplemented: []string{s3test.APILifecycle}})
	defer srv.Close()
	srv.CreateBucket("with-lifecycle")
	srv.CreateBucket("plain-bucket")
	cfg := writeS3Config(t, srv)
	dir := t.TempDir()
	intent := `{"resources": [
	  {"provider": "s3-bucket", "id": "fake/with-lifecycle", "fields": {"lifecycle": "[{\"ID\": \"x\", \"Status\": \"Enabled\", \"Expiration\": {\"Days\": 1}}]"}},
	  {"provider": "s3-bucket", "id": "fake/plain-bucket", "fields": {}}
	]}`
	if err := os.MkdirAll(filepath.Join(dir, "intent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent", "b.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(buildBinary(t), "status", "--json", "--intent", filepath.Join(dir, "intent"),
		"--state", filepath.Join(dir, "state.json"), "--s3-config", cfg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, _ := cmd.Output()
	var doc struct {
		Summary struct {
			Conformant   int `json:"conformant"`
			Unobservable int `json:"unobservable"`
		} `json:"summary"`
		Resources []struct {
			LocalID      string `json:"local_id"`
			State        string `json:"state"`
			ObserveError string `json:"observe_error"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("[AC-122-4] status --json: %v\n%s\n%s", err, out, stderr.String())
	}
	if doc.Summary.Unobservable != 1 || doc.Summary.Conformant != 1 {
		t.Fatalf("[AC-122-4] summary = %+v; want 1 unobservable and 1 conformant\n%s\n%s", doc.Summary, out, stderr.String())
	}
	for _, r := range doc.Resources {
		if r.LocalID == "fake/with-lifecycle" && (r.State != "unobservable" || !strings.Contains(r.ObserveError, "GetBucketLifecycleConfiguration (NotImplemented)")) {
			t.Errorf("[AC-122-4] fake/with-lifecycle = %+v; want unobservable naming the NotImplemented API", r)
		}
	}
}
