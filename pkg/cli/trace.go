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
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/otlpexport"
	"github.com/tjst-t/sakutei/v2/internal/tracing"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// traceFlushTimeout is how long an exit waits for the spans still queued.
const traceFlushTimeout = 5 * time.Second

// traceFlags are the trace exit flags (#189, §26.8): traces are sent over
// OTLP/HTTP only to an endpoint given explicitly; nothing is read from the
// OTEL_* environment.
type traceFlags struct {
	endpoint, config *string
	insecure         *bool
}

func addTraceFlags(fs *flag.FlagSet) *traceFlags {
	return &traceFlags{
		endpoint: fs.String("otlp-endpoint", "",
			"#189: send traces (the cycle, Observe, plan, Apply and Workflow steps as spans) over OTLP/HTTP to this receiver, e.g. https://collector.example:4318 (/v1/traces is appended); empty=no traces (the default)"),
		config: fs.String("otlp-config", "",
			"#189: TLS (CA, server name, TLS 1.2+, client certificate SecretRef) and authorization (bearer token SecretRef) of --otlp-endpoint (docs/tracing.md)"),
		insecure: fs.Bool("otlp-insecure", false,
			"#189: allow a non-loopback --otlp-endpoint without TLS (https) and authorization (refused otherwise)"),
	}
}

// start checks the flags (exit 1 on a refusal) and, when an endpoint is
// given, starts the exporter and returns a context that makes spans.  With a
// root name, the run is one trace under a root span of that name (plan,
// reconcile); without one (the daemon) every cycle is its own trace.  The root
// span is ended and the queued spans are sent when the run exits through
// exitAfterEvents.
func (f *traceFlags) start(prog, command, root string) context.Context {
	ctx := context.Background()
	cfg, err := otlpexport.Load(*f.endpoint, *f.config, *f.insecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		os.Exit(1)
	}
	if cfg.URL == nil {
		return ctx
	}
	exp, err := otlpexport.New(cfg, otlpexport.Resource{ServiceVersion: version,
		Attrs: []tracing.Attr{tracing.String("sakutei.command", command)}}, logErr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		os.Exit(1)
	}
	fmt.Fprintf(logErr, "%s: sending traces to %s over OTLP/HTTP\n", prog, cfg.Describe())
	ctx = tracing.WithTracer(ctx, tracing.NewTracer(exp))
	var span *tracing.Span
	if root != "" {
		ctx, span = tracing.Start(ctx, root)
	}
	exitHooks = append(exitHooks, func() {
		span.SetAttributes(tracing.Int("sakutei.exit_code", exitCode))
		if exitCode == 1 {
			span.Fail(fmt.Errorf("%s exited 1", prog))
		}
		span.End()
		exp.Shutdown(traceFlushTimeout)
	})
	return ctx
}

// tracedRead runs read (one resource's Observe) as a sakutei.read span.
func tracedRead(ctx context.Context, id core.ID, read func(context.Context) (core.Observation, error)) (core.Observation, error) {
	ctx, span := tracing.Start(ctx, tracing.SpanRead,
		tracing.String(tracing.AttrProvider, id.Provider), tracing.String(tracing.AttrResource, id.String()))
	defer span.End()
	obs, err := read(ctx)
	span.Fail(err)
	if err == nil {
		span.SetAttributes(tracing.String(tracing.AttrOutcome, obs.Availability.String()))
	}
	return obs, err
}
