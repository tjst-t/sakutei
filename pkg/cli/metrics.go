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

package cli

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/listenguard"
	"github.com/tjst-t/sakutei/v2/internal/metrics"
	"github.com/tjst-t/sakutei/v2/internal/readapi"
)

// listenAuth is the TLS and authorization of the read listeners (#161):
// --listen-auth-config and the --metrics-insecure-listen /
// --read-insecure-listen flags.  The approval API (--approval-listen) is not
// one of them: it shares neither the port nor the credentials.
type listenAuth struct {
	config          *string
	metricsInsecure *bool
	readInsecure    *bool

	cfg     listenguard.Config
	rejects *listenguard.Rejects
}

func addListenAuthFlags(fs *flag.FlagSet) *listenAuth {
	return &listenAuth{
		config: fs.String("listen-auth-config", "",
			"#161: TLS (certificate and key as SecretRefs, TLS 1.2+) and authorization (bearer token SecretRef, client certificate CA with allowed subjects / SANs) of --metrics-listen and --read-listen (docs/metrics.md)"),
		metricsInsecure: fs.Bool("metrics-insecure-listen", false,
			"#161: allow --metrics-listen on a non-loopback address without TLS and authorization (refused otherwise)"),
		readInsecure: fs.Bool("read-insecure-listen", false,
			"#161: allow --read-listen on a non-loopback address without TLS and authorization (refused otherwise)"),
		rejects: listenguard.NewRejects(),
	}
}

// check loads the config and refuses what may not be served: a listener
// configured but not served, a non-loopback address without TLS and
// authorization unless its insecure flag is given, and an approval API on the
// address of a read listener.  It exits 1 on a refusal.
func (a *listenAuth) check(prog, metricsAddr, readAddr, approvalAddr string) {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, prog+": "+format+"\n", args...)
		os.Exit(1)
	}
	cfg, err := listenguard.LoadConfig(*a.config)
	if err != nil {
		fail("%v", err)
	}
	a.cfg = cfg
	for _, l := range []struct {
		name, flag, insecureFlag, addr string
		insecure                       bool
		conf                           listenguard.Listener
	}{
		{"metrics", "metrics-listen", "metrics-insecure-listen", metricsAddr, *a.metricsInsecure, cfg.Metrics},
		{"read", "read-listen", "read-insecure-listen", readAddr, *a.readInsecure, cfg.Read},
	} {
		if l.addr == "" {
			if l.conf.Configured() {
				fail("--listen-auth-config: %q is configured but --%s is not given", l.name, l.flag)
			}
			if l.insecure {
				fail("--%s needs --%s", l.insecureFlag, l.flag)
			}
			continue
		}
		if err := listenguard.CheckAddr(l.flag, l.insecureFlag, l.addr, l.conf, l.insecure); err != nil {
			fail("%v", err)
		}
		if approvalAddr != "" && approvalAddr == l.addr {
			fail("--approval-listen and --%s are the same address %s: the approval API (writes) shares neither the port nor the credentials of the read listeners", l.flag, l.addr)
		}
	}
}

func (a *listenAuth) serve(name, addr string, conf listenguard.Listener, h http.Handler) (*listenguard.Served, error) {
	return listenguard.Serve(listenguard.Options{
		Name: name, Addr: addr, Handler: h, Listener: conf,
		SecretStore: a.cfg.SecretStore, Rejects: a.rejects,
		Logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, "sakutei: "+format+"\n", args...) },
	})
}

// serveMetrics serves the daemon's metrics on addr/metrics in the background
// (#54, §27.5), with the TLS and authorization of --listen-auth-config (#161).
// Time series and alerting stay with Prometheus (§27.6).
func serveMetrics(addr string, e *metrics.Exporter, a *listenAuth) error {
	e.AddFamilies(a.rejects.Families)
	s, err := a.serve("metrics", addr, a.cfg.Metrics, e.Handler())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "sakutei: metrics listening on %s/metrics (Prometheus text format; %s)\n", s.URL, s.Auth)
	return nil
}

// serveReadAPI serves the read-only API (#136) on addr in the background.  It
// is a listener of its own, apart from the approval API (--approval-listen),
// and takes no writes; its TLS and authorization are --listen-auth-config's
// (#161, docs/read-api.md).
func serveReadAPI(addr string, s *readapi.Server, a *listenAuth) error {
	sv, err := a.serve("read", addr, a.cfg.Read, s.Handler())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "sakutei: read-only API listening on %s (GET /status /plan /events /graph; %s)\n", sv.URL, sv.Auth)
	return nil
}

// budgetWindow is the rolling window the budget engine uses for cfg.
func budgetWindow(cfg budget.Config) time.Duration {
	if cfg.WindowDuration > 0 {
		return cfg.WindowDuration
	}
	return budget.DefaultWindowDuration
}
