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

// postgresql_secret_test.go — Backlog #137: when the admin connection's
// password (a SecretRef) cannot be resolved, the postgresql provider stops
// safely, as the other providers do.
//
// AC tags:
//
//	AC-137-1  the provider is built (the daemon builds it once) and then the
//	          admin secret leaves the store: Read is unknown and Apply fails
//	          with a TransientError (never a PermanentError, which the daemon
//	          would hold as stuck), nothing reaches the server, and once the
//	          secret is back the same Apply goes through
//	AC-137-2  plan / reconcile with --postgresql-config whose admin secret is
//	          not in the store stop before anything (non-zero exit, no change
//	          on the server, no secret in the output); with the secret back,
//	          reconcile converges
package acceptance

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/postgresql"
	"github.com/tjst-t/sakutei/internal/postgresql/pgtest"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

const pgStoreWithAdmin = `{"secrets": {"pg/admin": {"gen": 1, "value": "` + pgAdminSecret + `"}, "pg/app": {"gen": 1, "value": "app-role-secret"}}}`
const pgStoreWithoutAdmin = `{"secrets": {"pg/app": {"gen": 1, "value": "app-role-secret"}}}`

func (l pgLab) writeStore(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(l.config), "secrets.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPostgreSQL_AdminSecretGoneIsTransient(t *testing.T) {
	// [AC-137-1]
	ctx := context.Background()
	lab := newPGLab(t)
	p := lab.provider(t)
	id := core.NewID(postgresql.Name, core.NewScope(nil), "main/role/app")
	desired := diff.DesiredResource{ID: id, Fields: map[string]string{postgresql.FieldLogin: "true"}}
	obs, err := p.Read(ctx, id.Local)
	if err != nil {
		t.Fatalf("[AC-137-1] Read: %v", err)
	}
	plan := diff.ForProvider(p).Diff(obs, desired, cause.CauseContext{})
	if len(plan.Ops) != 1 || plan.Ops[0].Kind != core.OpKindCreate {
		t.Fatalf("[AC-137-1] plan = %v; want one Create", plan.Ops)
	}

	lab.writeStore(t, pgStoreWithoutAdmin)
	obs, err = p.Read(ctx, id.Local)
	if err == nil || obs.Availability != core.AvailabilityUnknown || sdk.IsPermanent(err) {
		t.Errorf("[AC-137-1] Read without the admin secret = %+v, %v; want Unknown with a non-permanent error", obs, err)
	}
	err = p.Apply(ctx, plan)
	if err == nil || !sdk.IsTransient(err) || sdk.IsPermanent(err) {
		t.Fatalf("[AC-137-1] Apply without the admin secret: %v; want a TransientError", err)
	}
	if !strings.Contains(err.Error(), "password cannot be resolved") || strings.Contains(err.Error(), pgAdminSecret) {
		t.Errorf("[AC-137-1] Apply error = %q; want it to say the password cannot be resolved, without the value", err)
	}
	if st := lab.edit(t, func(*pgtest.State) {}); st.Roles["app"] != nil {
		t.Error("[AC-137-1] the role was created without the admin secret")
	}

	lab.writeStore(t, pgStoreWithAdmin)
	if err := p.Apply(ctx, plan); err != nil {
		t.Fatalf("[AC-137-1] Apply with the secret back: %v", err)
	}
	obs, err = p.Read(ctx, id.Local)
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("[AC-137-1] Read with the secret back = %+v, %v", obs, err)
	}
	if pl := diff.ForProvider(p).Diff(obs, desired, cause.CauseContext{}); len(pl.Ops) != 0 {
		t.Errorf("[AC-137-1] after the secret is back: plan = %v; want converged", pl.Ops)
	}
}

func TestPostgreSQL_CLIAdminSecretMissingStops(t *testing.T) {
	// [AC-137-2]
	lab := newPGLab(t)
	dir := t.TempDir()
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `{"resources": [{"provider": "postgresql", "id": "main/role/app", "fields": {"login": "true"}}]}`
	if err := os.WriteFile(filepath.Join(intentDir, "pg.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		args = append(args, "--intent", intentDir, "--state", filepath.Join(dir, "state.json"), "--postgresql-config", lab.config)
		out, err := exec.Command(buildBinary(t), args...).CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if strings.Contains(string(out), pgAdminSecret) {
			t.Fatalf("[AC-137-2] sakutei %s shows the admin secret:\n%s", args[0], out)
		}
		return string(out), code
	}

	lab.writeStore(t, pgStoreWithoutAdmin)
	for _, verb := range []string{"plan", "reconcile"} {
		out, code := run(verb)
		if code == 0 || !strings.Contains(out, "pg/admin") {
			t.Errorf("[AC-137-2] %s without the admin secret exited %d; want non-zero naming the SecretRef:\n%s", verb, code, out)
		}
	}
	if st := lab.edit(t, func(*pgtest.State) {}); st.Roles["app"] != nil {
		t.Error("[AC-137-2] the role was created without the admin secret")
	}

	lab.writeStore(t, pgStoreWithAdmin)
	if out, code := run("reconcile"); code != 0 {
		t.Fatalf("[AC-137-2] reconcile with the secret back exited %d:\n%s", code, out)
	}
	if st := lab.edit(t, func(*pgtest.State) {}); st.Roles["app"] == nil || !st.Roles["app"].Login {
		t.Error("[AC-137-2] the role was not created once the secret was back")
	}
}
