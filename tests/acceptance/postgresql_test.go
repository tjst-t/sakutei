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

// postgresql_test.go — Backlog #125: the postgresql provider (roles,
// databases, database privileges, extensions) against the fake psql
// (internal/postgresql/pgtest, built as an executable so the provider runs
// it through os/exec exactly as it runs the real psql).  A real PostgreSQL is
// exercised by tests/integration/postgresql (make integration-postgresql).
//
// AC tags:
//
//	AC-125-1  DefaultRegistry builds postgresql from postgresql_config (the
//	          server list; the admin password is a SecretRef resolved from the
//	          file SecretBackend); without it is an error; the provider passes
//	          sdk.ContractTester (incl. unreachable → unknown, never absent)
//	AC-125-2  prune under PruneWithinScope: an orphan database with user
//	          tables is never dropped (PermanentError) and stays; an empty
//	          orphan database and an orphan role go (destructive); system
//	          roles / databases and the admin role are never prune candidates
//	AC-125-3  an unreachable server halts prune (List is not complete) and the
//	          declared resources are unobservable (no op)
//	AC-125-4  sakutei reconcile / status with --postgresql-config: created from
//	          empty, then conformant, and the password never appears in the
//	          output
package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/postgresql"
	"github.com/tjst-t/sakutei/internal/postgresql/pgtest"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

const pgAdminSecret = "pg-admin-secret-value"

var (
	fakePsqlOnce sync.Once
	fakePsqlPath string
	fakePsqlErr  error
)

// fakePsql builds the fake psql executable once.
func fakePsql(t *testing.T) string {
	t.Helper()
	fakePsqlOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			fakePsqlErr = err
			return
		}
		fakePsqlPath = filepath.Join(binaryDir, "fakepsql")
		cmd := exec.Command("go", "build", "-o", fakePsqlPath, "./internal/postgresql/pgtest/fakepsql")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			fakePsqlErr = errors.New(string(out))
		}
	})
	if fakePsqlErr != nil {
		t.Fatalf("build fakepsql: %v", fakePsqlErr)
	}
	return fakePsqlPath
}

// pgLab is a fake server (its state file) and a server list pointing at it.
type pgLab struct {
	state, config string
}

func newPGLab(t *testing.T) pgLab {
	t.Helper()
	dir := t.TempDir()
	lab := pgLab{state: filepath.Join(dir, "state.json"), config: filepath.Join(dir, "pg.yaml")}
	if err := pgtest.SaveState(lab.state, pgtest.NewState("sakutei", pgAdminSecret)); err != nil {
		t.Fatal(err)
	}
	store := `{"secrets": {"pg/admin": {"gen": 1, "value": "` + pgAdminSecret + `"}, "pg/app": {"gen": 1, "value": "app-role-secret"}}}`
	cfg := "psql: " + fakePsql(t) + "\nsecret_store: secrets.json\nservers:\n  main:\n    host: db.test\n    user: sakutei\n" +
		"    password: secretref:file/pg/admin\n    verify_passwords: true\n"
	for name, body := range map[string]string{"secrets.json": store, "pg.yaml": cfg} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SAKUTEI_FAKE_PSQL_STATE", lab.state)
	return lab
}

func (l pgLab) edit(t *testing.T, f func(*pgtest.State)) *pgtest.State {
	t.Helper()
	st, err := pgtest.LoadState(l.state)
	if err != nil {
		t.Fatal(err)
	}
	f(st)
	if err := pgtest.SaveState(l.state, st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (l pgLab) provider(t *testing.T) provider.Provider {
	t.Helper()
	p, err := registry.DefaultRegistry().Get(postgresql.Name, map[string]string{"postgresql_config": l.config})
	if err != nil {
		t.Fatalf("DefaultRegistry().Get(postgresql): %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	return p
}

func TestPostgreSQL_RegistryAndContract(t *testing.T) {
	// [AC-125-1]
	if _, err := registry.DefaultRegistry().Get(postgresql.Name, map[string]string{"state": "x.json"}); err == nil || !strings.Contains(err.Error(), "--postgresql-config") {
		t.Errorf("[AC-125-1] postgresql without postgresql_config: err = %v; want one naming --postgresql-config", err)
	}
	lab := newPGLab(t)
	p := lab.provider(t)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p.(provider.Lister),
		Scope:    core.NewScope(nil),
		SampleID: "main/role/ac125_contract",
		SampleDesired: map[string]string{
			postgresql.FieldLogin:           "true",
			postgresql.FieldConnectionLimit: "3",
			postgresql.FieldPassword:        "secretref:file/pg/app#gen1",
		},
		MakeUnreachable: func() (func(), error) {
			lab.edit(t, func(st *pgtest.State) { st.Unreachable = true })
			return func() { lab.edit(t, func(st *pgtest.State) { st.Unreachable = false }) }, nil
		},
	}
	ct.RunAll(t)
}

func TestPostgreSQL_PruneKeepsDatabasesWithTables(t *testing.T) {
	// [AC-125-2]
	ctx := context.Background()
	lab := newPGLab(t)
	p := lab.provider(t)
	lab.edit(t, func(st *pgtest.State) {
		st.Roles["app"] = &pgtest.Role{Login: true, Inherit: true, ConnLimit: -1}
		st.Roles["orphan_role"] = &pgtest.Role{Inherit: true, ConnLimit: -1}
		for _, n := range []string{"appdb", "orphan_full", "orphan_empty"} {
			st.Databases[n] = &pgtest.Database{Owner: "app", Encoding: "UTF8", AllowConn: true, ConnLimit: -1, Extensions: map[string]string{"plpgsql": "1.0"}}
		}
		st.Databases["orphan_full"].Tables = 1
	})
	scope := core.NewScope(nil)
	desired := []diff.DesiredResource{
		{ID: core.NewID(postgresql.Name, scope, "main/role/app"), Fields: map[string]string{postgresql.FieldLogin: "true"}, DeletionPolicy: core.PruneWithinScope},
		{ID: core.NewID(postgresql.Name, scope, "main/database/appdb"), Fields: map[string]string{postgresql.FieldOwner: "app"}, DeletionPolicy: core.PruneWithinScope},
	}
	if err := prune.ValidatePrunePolicy(desired, p); err != nil {
		t.Fatalf("[AC-125-2] ValidatePrunePolicy: %v", err)
	}
	res, err := prune.New().ComputePrune(ctx, p.(provider.Lister), p, scope, scope, desired, nil, io.Discard)
	if err != nil || res.Halted {
		t.Fatalf("[AC-125-2] ComputePrune = %+v, %v", res, err)
	}
	var targets []string
	for _, op := range res.DeleteOps {
		targets = append(targets, op.Target.Local)
		if !op.Destructive {
			t.Errorf("[AC-125-2] Delete of %s is not destructive (it must count against the budget)", op.Target)
		}
	}
	if strings.Join(targets, " ") != "main/role/orphan_role main/database/orphan_empty main/database/orphan_full" {
		t.Fatalf("[AC-125-2] DeleteOps = %v; want the three orphans and nothing of the system", targets)
	}
	for _, op := range res.DeleteOps {
		err := p.Apply(ctx, core.Plan{Ops: []core.Op{op}})
		var pe *provider.PermanentError
		switch op.Target.Local {
		case "main/database/orphan_full":
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), "user table") {
				t.Errorf("[AC-125-2] Delete of the database with a table: %v; want a PermanentError", err)
			}
		default:
			if err != nil {
				t.Errorf("[AC-125-2] Delete of %s: %v", op.Target.Local, err)
			}
		}
	}
	st := lab.edit(t, func(*pgtest.State) {})
	if _, ok := st.Databases["orphan_full"]; !ok {
		t.Error("[AC-125-2] the database with a table was dropped")
	}
	if _, ok := st.Databases["orphan_empty"]; ok {
		t.Error("[AC-125-2] the empty orphan database was not dropped")
	}
	if _, ok := st.Roles["orphan_role"]; ok {
		t.Error("[AC-125-2] the orphan role was not dropped")
	}
	for _, n := range []string{"postgres", "template0", "template1"} {
		if _, ok := st.Databases[n]; !ok {
			t.Errorf("[AC-125-2] %s was dropped", n)
		}
	}
}

func TestPostgreSQL_UnreachableDoesNothing(t *testing.T) {
	// [AC-125-3]
	ctx := context.Background()
	lab := newPGLab(t)
	p := lab.provider(t)
	lab.edit(t, func(st *pgtest.State) {
		st.Roles["orphan_role"] = &pgtest.Role{Inherit: true, ConnLimit: -1}
		st.Unreachable = true
	})
	id := core.NewID(postgresql.Name, core.NewScope(nil), "main/role/declared")
	desired := []diff.DesiredResource{{ID: id, Fields: map[string]string{postgresql.FieldLogin: "true"}, DeletionPolicy: core.PruneWithinScope}}
	obs, err := p.Read(ctx, id.Local)
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("[AC-125-3] Read = %+v, %v; want Unknown", obs, err)
	}
	if pl := diff.ForProvider(p).Diff(obs, desired[0], cause.CauseContext{}); len(pl.Ops) != 0 {
		t.Errorf("[AC-125-3] plan while unreachable = %v; want nothing", pl.Ops)
	}
	res, err := prune.New().ComputePrune(ctx, p.(provider.Lister), p, core.NewScope(nil), core.NewScope(nil), desired, nil, io.Discard)
	if err != nil || !res.Halted || len(res.DeleteOps) != 0 {
		t.Errorf("[AC-125-3] ComputePrune while unreachable = %+v, %v; want halted with no Delete", res, err)
	}
	st := lab.edit(t, func(st *pgtest.State) { st.Unreachable = false })
	if _, ok := st.Roles["orphan_role"]; !ok {
		t.Error("[AC-125-3] a role was dropped while the server was unreachable")
	}
}

func TestPostgreSQL_CLIReconcileAndStatus(t *testing.T) {
	// [AC-125-4]
	lab := newPGLab(t)
	dir := t.TempDir()
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `{"resources": [
	  {"provider": "postgresql", "id": "main/role/app", "fields": {"login": "true", "password": "secretref:file/pg/app#gen1", "member_of": "pg_monitor"}},
	  {"provider": "postgresql", "id": "main/database/appdb", "fields": {"owner": "app", "connection_limit": "20"}},
	  {"provider": "postgresql", "id": "main/database/appdb/grant/app", "fields": {"privileges": "CONNECT,CREATE"}},
	  {"provider": "postgresql", "id": "main/database/appdb/extension/citext", "fields": {}}
	]}`
	if err := os.WriteFile(filepath.Join(intentDir, "pg.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		args = append(args, "--intent", intentDir, "--state", filepath.Join(dir, "state.json"), "--postgresql-config", lab.config)
		cmd := exec.Command(buildBinary(t), args...)
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		for _, s := range []string{pgAdminSecret, "app-role-secret", "SCRAM-SHA-256$"} {
			if strings.Contains(string(out), s) {
				t.Fatalf("[AC-125-4] sakutei %s shows a secret (%q):\n%s", args[0], s, out)
			}
		}
		return string(out), code
	}
	// 1 周で全部できる（拡張・権限はデータベースの後。同じ周にできなければ次の周）。
	for i := 0; i < 3; i++ {
		if out, code := run("reconcile"); code != 0 {
			t.Fatalf("[AC-125-4] reconcile #%d exited %d:\n%s", i+1, code, out)
		}
	}
	out, code := run("status", "--json")
	if code != 0 {
		t.Fatalf("[AC-125-4] status exited %d:\n%s", code, out)
	}
	var doc struct {
		Summary struct {
			Conformant   int `json:"conformant"`
			Drift        int `json:"drift"`
			Unobservable int `json:"unobservable"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &doc); err != nil {
		t.Fatalf("[AC-125-4] status --json: %v\n%s", err, out)
	}
	if doc.Summary.Conformant != 4 || doc.Summary.Drift != 0 || doc.Summary.Unobservable != 0 {
		t.Errorf("[AC-125-4] summary = %+v; want 4 conformant\n%s", doc.Summary, out)
	}
	st := lab.edit(t, func(*pgtest.State) {})
	if r := st.Roles["app"]; r == nil || !r.Login || !postgresql.PasswordMatches(r.Password, "app", "app-role-secret") {
		t.Errorf("[AC-125-4] role app = %+v", r)
	}
	if st.PlaintextPasswords != 0 {
		t.Errorf("[AC-125-4] a plaintext password reached the server")
	}
}
