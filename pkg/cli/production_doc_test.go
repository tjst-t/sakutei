// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cli

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/listenguard"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/otlpexport"
	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/runtimeargs"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
)

// docMainEnv makes the test binary run Main (the sakutei CLI) instead of the
// tests, so the guide's command lines run against the real flag sets.
const docMainEnv = "SAKUTEI_DOC_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(docMainEnv) == "1" {
		Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const productionDoc = "../../docs/production.md"

// docPrefixes are the places the guide's examples live; the test moves them
// under a temporary directory.
var docPrefixes = []string{"/etc/sakutei", "/var/lib/sakutei", "/srv/sakutei", "/run/sakutei"}

type docBlock struct {
	check string // the text of <!-- check: … -->
	body  string
	line  int
}

var checkMarker = regexp.MustCompile(`^<!-- check: (.+) -->$`)

// readDocBlocks returns the fenced blocks of the guide marked with a check,
// and fails on a yaml block without one (every configuration example is
// checked).
func readDocBlocks(t *testing.T) []docBlock {
	t.Helper()
	data, err := os.ReadFile(productionDoc)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	var out []docBlock
	check := ""
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if m := checkMarker.FindStringSubmatch(l); m != nil {
			check = m[1]
			continue
		}
		if !strings.HasPrefix(l, "```") {
			if strings.TrimSpace(l) != "" {
				check = ""
			}
			continue
		}
		lang := strings.TrimPrefix(l, "```")
		start := i + 1
		for i++; i < len(lines) && lines[i] != "```"; i++ {
		}
		if check == "" {
			if lang == "yaml" {
				t.Errorf("%s:%d: a yaml example without <!-- check: … -->", productionDoc, start)
			}
			continue
		}
		out = append(out, docBlock{check: check, body: strings.Join(lines[start:i], "\n") + "\n", line: start})
		check = ""
	}
	return out
}

// docCommands splits a block into the sakutei command lines ("$ sakutei …"
// or "ExecStart=…/sakutei …", with "\" continuations), each as its words.
func docCommands(t *testing.T, b docBlock) [][]string {
	t.Helper()
	var out [][]string
	text := strings.ReplaceAll(b.body, "\\\n", " ")
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		l = strings.TrimPrefix(l, "$ ")
		l = strings.TrimPrefix(l, "ExecStart=")
		words := splitWords(t, l)
		if len(words) == 0 || filepath.Base(words[0]) != "sakutei" {
			continue
		}
		out = append(out, words[1:])
	}
	if len(out) == 0 {
		t.Fatalf("%s:%d: no sakutei command in the block", productionDoc, b.line)
	}
	return out
}

// splitWords splits a command line on blanks, keeping "…" together.
func splitWords(t *testing.T, s string) []string {
	var out []string
	var cur strings.Builder
	in, quoted := false, false
	for _, r := range s {
		switch {
		case r == '"':
			quoted, in = !quoted, true
		case (r == ' ' || r == '\t') && !quoted:
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quoted {
		t.Fatalf("unbalanced quote in %q", s)
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// flagValues returns the --name value pairs of a command.
func flagValues(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") {
			continue
		}
		name := strings.TrimPrefix(args[i], "--")
		if k, v, ok := strings.Cut(name, "="); ok {
			out[k] = v
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[name] = args[i+1]
			i++
		} else {
			out[name] = "true"
		}
	}
	return out
}

// verbOf returns the subcommand words of a command (none for the daemon).
func verbOf(args []string) []string {
	var v []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || len(v) == 2 {
			break
		}
		v = append(v, a)
	}
	if len(v) == 2 && v[0] != "freeze" && v[0] != "audit" {
		v = v[:1]
	}
	return v
}

// runSakuteiMain runs the CLI (this test binary as sakutei) with args.
func runSakuteiMain(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), docMainEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sakutei %v: %v", args, err)
	}
	return out.String(), 0
}

// checkFlagNames fails on a flag of the command that its subcommand does not
// have (read from its -h).
func checkFlagNames(t *testing.T, b docBlock, args []string, help map[string]string) {
	t.Helper()
	verb := verbOf(args)
	key := strings.Join(verb, " ")
	h, ok := help[key]
	if !ok {
		h, _ = runSakuteiMain(t, append(append([]string{}, verb...), "-h")...)
		help[key] = h
	}
	for name := range flagValues(args) {
		if !regexp.MustCompile(`(?m)^\s+-` + regexp.QuoteMeta(name) + `(\s|$)`).MatchString(h) {
			t.Errorf("%s:%d: sakutei %s has no flag --%s", productionDoc, b.line, key, name)
		}
	}
}

type docFixtures struct {
	root        string
	caPEM       string
	certPEM     string
	keyPEM      string
	secretPaths []string
}

func (f *docFixtures) path(p string) string {
	for _, pre := range docPrefixes {
		p = strings.ReplaceAll(p, pre, filepath.Join(f.root, pre))
	}
	return p
}

func newDocFixtures(t *testing.T) *docFixtures {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "doc-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "sakutei"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"sakutei.example.net"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	f := &docFixtures{root: t.TempDir(),
		caPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))}
	for _, d := range []string{"/etc/sakutei", "/var/lib/sakutei/audit", "/srv/sakutei/intent", "/run/sakutei"} {
		if err := os.MkdirAll(f.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *docFixtures) write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(f.path(p), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeAux writes the files the examples point at that the guide does not
// show: CA bundles, certificates, known_hosts and the AppRole files.
func (f *docFixtures) writeAux(t *testing.T, texts []string) {
	t.Helper()
	ref := regexp.MustCompile(`/etc/sakutei/[A-Za-z0-9._-]+`)
	seen := map[string]bool{}
	for _, s := range texts {
		for _, p := range ref.FindAllString(s, -1) {
			if seen[p] {
				continue
			}
			seen[p] = true
			if _, err := os.Stat(f.path(p)); err == nil || strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".json") {
				continue
			}
			switch {
			case strings.HasSuffix(p, "-ca.pem"):
				f.write(t, p, f.caPEM)
			case strings.HasSuffix(p, "-key.pem"):
				f.write(t, p, f.keyPEM)
			case strings.HasSuffix(p, ".pem"):
				f.write(t, p, f.certPEM)
			case strings.HasSuffix(p, "known_hosts"):
				f.write(t, p, "192.0.2.11 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA\n")
			default:
				f.write(t, p, "doc-"+filepath.Base(p)+"\n")
			}
		}
	}
}

// writeStore writes a file SecretBackend at p with the refs of the examples
// whose path keep accepts: certificates for *_cert, keys for *_key, a token
// otherwise.
func (f *docFixtures) writeStore(t *testing.T, p string, texts []string, keep func(string) bool) {
	t.Helper()
	ref := regexp.MustCompile(`secretref:file/([A-Za-z0-9/._-]+)`)
	secrets := map[string]map[string]any{}
	for _, s := range texts {
		for _, m := range ref.FindAllStringSubmatch(s, -1) {
			if !keep(m[1]) {
				continue
			}
			v := "doc-token-" + m[1]
			switch {
			case strings.HasSuffix(m[1], "_cert"):
				v = f.certPEM
			case strings.HasSuffix(m[1], "_key"):
				v = f.keyPEM
			}
			secrets[m[1]] = map[string]any{"gen": 1, "value": v}
		}
	}
	data, err := json.Marshal(map[string]any{"secrets": secrets})
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, p, string(data))
}

// #190: the configuration examples of the production guide (docs/production.md)
// load as written, its command lines use flags their commands have, and the
// freeze and audit commands run.
func TestProductionGuideExamples(t *testing.T) {
	blocks := readDocBlocks(t)
	f := newDocFixtures(t)

	files := map[string]string{} // path in the guide → body
	var texts []string
	var commands []struct {
		b    docBlock
		args []string
	}
	for _, b := range blocks {
		kind, arg, _ := strings.Cut(b.check, " ")
		switch kind {
		case "file":
			if _, dup := files[arg]; dup {
				t.Fatalf("%s:%d: %s is shown twice", productionDoc, b.line, arg)
			}
			files[arg] = b.body
			texts = append(texts, b.body)
		case "flags", "run", "runtime-tightens", "runtime-refused":
			for _, args := range docCommands(t, b) {
				commands = append(commands, struct {
					b    docBlock
					args []string
				}{b, args})
				texts = append(texts, strings.Join(args, " "))
			}
		default:
			t.Fatalf("%s:%d: unknown check %q", productionDoc, b.line, b.check)
		}
	}
	for _, want := range []string{"/etc/sakutei/remote.yaml", "/etc/sakutei/pve.yaml", "/etc/sakutei/provider-config.yaml",
		"/etc/sakutei/listen-auth.yaml", "/etc/sakutei/otlp.yaml", "/srv/sakutei/intent/runtime.yaml", "/srv/sakutei/intent/providers.yaml"} {
		if _, ok := files[want]; !ok {
			t.Errorf("the guide shows no %s", want)
		}
	}
	for p, body := range files {
		f.write(t, p, f.path(body))
	}
	f.writeAux(t, texts)
	f.writeStore(t, "/etc/sakutei/secrets.json", texts, func(string) bool { return true })

	// intent（runtime:・providers:・SecretRef の取り出し先を使うリソース）
	in, err := loader.LoadIntent(f.path("/srv/sakutei/intent"))
	if err != nil {
		t.Fatalf("the guide's intent does not load: %v", err)
	}
	if in.Runtime == nil || len(in.Providers) == 0 || len(in.Resources) == 0 {
		t.Fatalf("intent: runtime = %v, providers = %v, resources = %d", in.Runtime, in.Providers, len(in.Resources))
	}

	t.Run("remote.yaml: the write key is only needed by a run that writes", func(t *testing.T) {
		p := f.path("/etc/sakutei/remote.yaml")
		cfg, err := remote.LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Hosts) != 2 {
			t.Errorf("hosts = %v", cfg.Hosts)
		}
		// 運用者の PC：観測用の鍵だけの金庫で、同じ一覧（secret_store だけ違う）。
		f.writeStore(t, "/etc/sakutei/secrets-observe.json", texts, func(s string) bool { return strings.HasSuffix(s, "-ro") })
		obs := f.path("/etc/sakutei/remote-observe.yaml")
		body := strings.Replace(f.path(files["/etc/sakutei/remote.yaml"]), "secrets.json", "secrets-observe.json", 1)
		if err := os.WriteFile(obs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := remote.LoadConfigWith(obs, remote.LoadOptions{ObserveOnly: true}); err != nil {
			t.Errorf("observe with the observe keys only: %v", err)
		}
		if _, err := remote.LoadConfig(obs); err == nil || !strings.Contains(err.Error(), "write") {
			t.Errorf("a run that writes without the write keys: err = %v", err)
		}
	})

	t.Run("pve.yaml", func(t *testing.T) {
		if _, err := proxmox.LoadConfig(f.path("/etc/sakutei/pve.yaml")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("provider-config.yaml with the intent's providers:", func(t *testing.T) {
		p := f.path("/etc/sakutei/provider-config.yaml")
		reg := registry.DefaultRegistry()
		over, err := loadProviderConfig(p, reg)
		if err != nil {
			t.Fatal(err)
		}
		backends, err := loadSecretBackends(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := backends["vault"]; !ok {
			t.Errorf("secret_backends = %v", backends)
		}
		if err := reg.CheckIntentProviders(in.Providers); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(over))
		for n := range over {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, access := range []string{provider.AccessObserve, ""} {
				cfg := map[string]string{}
				for k, v := range over[n] {
					cfg[k] = v
				}
				if access != "" {
					cfg[provider.ConfigAccess] = access
				}
				_, warnings, err := reg.GetWithIntent(n, cfg, in.Providers[n])
				if err != nil {
					t.Errorf("provider %s (access %q): %v", n, access, err)
				}
				if len(warnings) > 0 {
					t.Errorf("provider %s: the range is only in the provider config: %v", n, warnings)
				}
			}
		}
	})

	daemon := map[string]string{}
	for _, c := range commands {
		if c.b.check == "flags" && len(verbOf(c.args)) == 0 {
			daemon = flagValues(c.args)
		}
	}
	if daemon["otlp-endpoint"] == "" || daemon["audit-syslog"] == "" {
		t.Fatalf("the guide's unit does not run the daemon with --otlp-endpoint and --audit-syslog: %v", daemon)
	}

	t.Run("listen-auth.yaml", func(t *testing.T) {
		cfg, err := listenguard.LoadConfig(f.path("/etc/sakutei/listen-auth.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Metrics.TLS == nil || cfg.Metrics.Token == nil || cfg.Read.TLS == nil || cfg.Read.ClientCAFile == "" {
			t.Errorf("listen auth = %+v", cfg)
		}
	})

	t.Run("otlp.yaml", func(t *testing.T) {
		if _, err := otlpexport.Load(daemon["otlp-endpoint"], f.path("/etc/sakutei/otlp.yaml"), false); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the unit's audit flags", func(t *testing.T) {
		fs := flag.NewFlagSet("unit", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		sink := addAuditSinkFlags(fs)
		var args []string
		for k, v := range daemon {
			if strings.HasPrefix(k, "audit-") && k != "audit-log" {
				args = append(args, "--"+k, f.path(v))
			}
		}
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if _, err := sink.open(f.path(daemon["audit-log"])); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("commands", func(t *testing.T) {
		help := map[string]string{}
		var runs [][]string
		for _, c := range commands {
			checkFlagNames(t, c.b, c.args, help)
			switch c.b.check {
			case "run":
				runs = append(runs, c.args)
			case "flags":
				if len(verbOf(c.args)) == 0 {
					res := runtimeFor(t, in, c.args)
					if len(res.Tightened) > 0 || len(res.Conflicts) > 0 {
						t.Errorf("%s:%d: the unit repeats runtime: arguments: %+v %+v", productionDoc, c.b.line, res.Tightened, res.Conflicts)
					}
				}
			case "runtime-tightens daemon":
				res := runtimeFor(t, in, c.args)
				if len(res.Conflicts) > 0 || len(res.Tightened) == 0 || len(res.Tightened) != countRuntimeFlags(c.args) {
					t.Errorf("%s:%d: want every runtime flag to tighten: tightened %+v, conflicts %+v", productionDoc, c.b.line, res.Tightened, res.Conflicts)
				}
			case "runtime-refused daemon":
				res := runtimeFor(t, in, c.args)
				if len(res.Conflicts) == 0 || len(res.Conflicts) != countRuntimeFlags(c.args) {
					t.Errorf("%s:%d: want every runtime flag refused: tightened %+v, conflicts %+v", productionDoc, c.b.line, res.Tightened, res.Conflicts)
				}
			}
		}
		// audit verify reads what the freeze commands recorded: run it last.
		sort.SliceStable(runs, func(i, j int) bool { return runs[i][0] != "audit" && runs[j][0] == "audit" })
		if len(runs) < 4 {
			t.Fatalf("runs = %v", runs)
		}
		for _, args := range runs {
			full := make([]string, len(args))
			for i, a := range args {
				full[i] = f.path(a)
			}
			out, code := runSakuteiMain(t, full...)
			if code != 0 {
				t.Errorf("sakutei %s: exit %d\n%s", strings.Join(args, " "), code, out)
			}
			if args[0] == "freeze" && args[1] == "status" && !strings.Contains(out, "alice") {
				t.Errorf("freeze status does not show the freeze:\n%s", out)
			}
		}
	})
}

// runtimeFor resolves the runtime arguments of a daemon command against the
// guide's runtime: for the daemon.
func runtimeFor(t *testing.T, in loader.Intent, args []string) runtimeargs.Result {
	t.Helper()
	flags := map[string]string{}
	for name, v := range flagValues(args) {
		a, ok := runtimeargs.ByFlag(name)
		if !ok {
			continue
		}
		n, err := a.Normalize(v)
		if err != nil {
			t.Fatalf("--%s %s: %v", name, v, err)
		}
		flags[a.Key] = n
	}
	return runtimeargs.Resolve(in.Runtime.For(runtimeargs.Daemon), flags, nil)
}

func countRuntimeFlags(args []string) int {
	n := 0
	for name := range flagValues(args) {
		if _, ok := runtimeargs.ByFlag(name); ok {
			n++
		}
	}
	return n
}
