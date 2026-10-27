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
	"io"
	"os"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/eventnotify"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// logOut / logErr are where the daemon, reconcile and plan write their
// notices (core.Event) and log lines: standard output / error, wrapped in the
// --log-format chosen (§22.3, §27.2).  Until setLogFormat runs they are text.
var (
	logOut io.Writer = os.Stdout
	logErr io.Writer = os.Stderr
)

const logFormatUsage = "output form of notices and log lines: text (default) or json (one core.Event per line, JSON lines; §22.3)"

// addLogFormatFlag registers --log-format on fs.
func addLogFormatFlag(fs *flag.FlagSet) *string {
	return fs.String("log-format", string(eventlog.FormatText), logFormatUsage)
}

// setLogFormat wraps logOut / logErr in the form named by value; it exits on
// an unknown form.
func setLogFormat(command, value string) {
	f, err := eventlog.ParseFormat(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		os.Exit(1)
	}
	logOut = eventlog.New(os.Stdout, f, core.SeverityInfo)
	logErr = eventlog.New(os.Stderr, f, core.SeverityWarning)
}

const eventLogUsage = "file to append every notice (core.Event) to as JSON lines, one Event per line, besides standard error (§22.3); empty=none. A write failure is logged and convergence goes on"

// addEventLogFlag registers --event-log on fs.
func addEventLogFlag(fs *flag.FlagSet) *string {
	return fs.String("event-log", "", eventLogUsage)
}

// openEventLog opens the --event-log file sink at path (nil when empty); it
// exits when the file cannot be opened.
func openEventLog(command, path string) []eventlog.Sink {
	if path == "" {
		return nil
	}
	s, err := eventlog.OpenFileSink(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		os.Exit(1)
	}
	return []eventlog.Sink{s}
}

// routeEventLog hands the Events written to logOut / logErr to the
// --event-log file at path and to the notification sinks of nf as well (the
// one-shot reconcile).  exitAfterEvents flushes the notification sinks.
func routeEventLog(command, path string, nf *eventNotifyFlags) {
	sinks := append(openEventLog(command, path), nf.open(command)...)
	errOut := logErr
	logOut = eventlog.Route(context.Background(), logOut, sinks, errOut)
	logErr = eventlog.Route(context.Background(), logErr, sinks, errOut)
}

// #131 (§22.3): the notification sinks — webhook and Alertmanager.

const (
	eventWebhookUsage      = "URL to POST every notice (core.Event) to as JSON, one Event per request (§22.3); empty=none. Sent in the background: a failure is logged and retried with a growing wait, and convergence goes on"
	eventAlertmanagerUsage = "Alertmanager base URL (alerts go to <url>/api/v2/alerts) to send the level notices (firing / resolved) to, resent while firing so they do not expire; warning+ edge notices go as alerts that end after edge_ttl (§22.3); empty=none. A failure is logged and retried; convergence goes on"
	eventNotifyConfigUsage = "file (YAML/JSON) with the headers (SecretRefs), TLS (ca_file, insecure_skip_verify), timeout and, for Alertmanager, resend_interval, edge_ttl and labels of --event-webhook / --event-alertmanager (§22.3)"
)

// notifyFlushTimeout bounds how long a one-shot command waits at exit for the
// notification sinks to deliver what is queued.
const notifyFlushTimeout = 10 * time.Second

// eventNotifyFlags are --event-webhook, --event-alertmanager and
// --event-notify-config.
type eventNotifyFlags struct {
	webhook, alertmanager, config *string
}

// addEventNotifyFlags registers the notification sink flags on fs.
func addEventNotifyFlags(fs *flag.FlagSet) *eventNotifyFlags {
	return &eventNotifyFlags{
		webhook:      fs.String("event-webhook", "", eventWebhookUsage),
		alertmanager: fs.String("event-alertmanager", "", eventAlertmanagerUsage),
		config:       fs.String("event-notify-config", "", eventNotifyConfigUsage),
	}
}

// notifyClosers flush and stop the notification sinks opened (exitAfterEvents).
var notifyClosers []func(context.Context) error

// open starts the notification sinks named by the flags; it exits on a bad
// URL or config.  Their delivery failures go to logErr.
func (nf *eventNotifyFlags) open(command string) []eventlog.Sink {
	if nf == nil || (*nf.webhook == "" && *nf.alertmanager == "") {
		if nf != nil && *nf.config != "" {
			fmt.Fprintf(os.Stderr, "%s: --event-notify-config needs --event-webhook or --event-alertmanager\n", command)
			os.Exit(1)
		}
		return nil
	}
	cfg, err := eventnotify.LoadConfig(*nf.config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		os.Exit(1)
	}
	opts := eventnotify.Options{ErrOut: logErr}
	var sinks []eventlog.Sink
	if *nf.webhook != "" {
		t := cfg.Webhook
		t.URL = *nf.webhook
		w, err := eventnotify.NewWebhook(t, cfg.SecretStore, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: --event-webhook: %v\n", command, err)
			os.Exit(1)
		}
		sinks = append(sinks, w)
		notifyClosers = append(notifyClosers, w.Close)
	}
	if *nf.alertmanager != "" {
		t := cfg.Alertmanager
		t.URL = *nf.alertmanager
		a, err := eventnotify.NewAlertmanager(t, cfg.SecretStore, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: --event-alertmanager: %v\n", command, err)
			os.Exit(1)
		}
		sinks = append(sinks, a)
		notifyClosers = append(notifyClosers, a.Close)
	}
	return sinks
}

// flushNotify gives the notification sinks up to notifyFlushTimeout to
// deliver what is queued; what is left is logged.
func flushNotify() {
	if len(notifyClosers) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyFlushTimeout)
	defer cancel()
	for _, c := range notifyClosers {
		if err := c(ctx); err != nil {
			fmt.Fprintf(logErr, "sakutei: warn: event sink: %v\n", err)
		}
	}
	notifyClosers = nil
}

// exitAfterEvents flushes the notification sinks, then exits with code.
func exitAfterEvents(code int) {
	flushNotify()
	os.Exit(code)
}
