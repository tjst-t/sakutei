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
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/metrics"
	"github.com/tjst-t/sakutei/internal/readapi"
)

// serveMetrics serves the daemon's metrics on addr/metrics in the background
// (#54, §27.5).  Like the approval API it has no authorization: bind it to a
// trusted address.  Time series and alerting stay with Prometheus (§27.6).
func serveMetrics(addr string, e *metrics.Exporter) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	srv := &http.Server{Handler: e.Handler(), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stdout, "sakutei: metrics listening on http://%s/metrics (Prometheus text format; no authorization)\n", ln.Addr())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "sakutei: metrics: %v\n", err)
		}
	}()
	return nil
}

// serveReadAPI serves the read-only API (#136) on addr in the background.  It
// is a listener of its own, apart from the approval API (--approval-listen),
// and takes no writes.  No authorization: bind it to a trusted address and put
// a proxy in front to expose it (docs/read-api.md).
func serveReadAPI(addr string, s *readapi.Server) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("read api: %w", err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stdout, "sakutei: read-only API listening on http://%s (GET /status /plan /events /graph; no authorization)\n", ln.Addr())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "sakutei: read api: %v\n", err)
		}
	}()
	return nil
}

// budgetWindow is the rolling window the budget engine uses for cfg.
func budgetWindow(cfg budget.Config) time.Duration {
	if cfg.WindowDuration > 0 {
		return cfg.WindowDuration
	}
	return budget.DefaultWindowDuration
}
