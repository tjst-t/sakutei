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
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/eventnotify"
	"github.com/tjst-t/sakutei/v2/internal/metrics"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
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

// #131 (§22.3): the notification sinks — webhook and Alertmanager.  #154:
// each takes several destinations, by repeating the flag or joining URLs with
// ","; every destination gets the same notices through a sink of its own.

const (
	eventWebhookUsage      = "URL to POST every notice (core.Event) to as JSON, one Event per request (§22.3); repeat the flag or join URLs with \",\" to send to several (#154); empty=none. Sent in the background: a failure is logged and retried with a growing wait, and convergence goes on"
	eventAlertmanagerUsage = "Alertmanager base URL (alerts go to <url>/api/v2/alerts) to send the level notices (firing / resolved) to, resent while firing so they do not expire; warning+ edge notices go as alerts that end after edge_ttl (§22.3). Repeat the flag or join URLs with \",\" to send the same alerts to every Alertmanager of a cluster, each with its own resend, retry and queue (#154); empty=none. A failure is logged and retried; convergence goes on"
	eventNotifyConfigUsage = "file (YAML/JSON) with the destinations (urls), headers (SecretRefs), TLS (ca_file, insecure_skip_verify), timeout and, for Alertmanager, resend_interval, edge_ttl and labels of --event-webhook / --event-alertmanager (§22.3)"
)

// notifyFlushTimeout bounds how long a one-shot command waits at exit for the
// notification sinks to deliver what is queued.
const notifyFlushTimeout = 10 * time.Second

// urlList is a flag that may be repeated; each value may join URLs with ",".
type urlList []string

func (l *urlList) String() string { return strings.Join(*l, ",") }

func (l *urlList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// eventNotifyFlags are --event-webhook, --event-alertmanager and
// --event-notify-config.  set is the sinks open() started.
type eventNotifyFlags struct {
	webhook, alertmanager urlList
	config                *string
	set                   *eventnotify.Set
}

// addEventNotifyFlags registers the notification sink flags on fs.
func addEventNotifyFlags(fs *flag.FlagSet) *eventNotifyFlags {
	nf := &eventNotifyFlags{config: fs.String("event-notify-config", "", eventNotifyConfigUsage)}
	fs.Var(&nf.webhook, "event-webhook", eventWebhookUsage)
	fs.Var(&nf.alertmanager, "event-alertmanager", eventAlertmanagerUsage)
	return nf
}

// notifyClosers flush and stop the notification sinks opened (exitAfterEvents).
var notifyClosers []func(context.Context) error

// open starts the notification sinks named by the flags and the config's
// urls; it exits on a bad URL or config.  Their delivery failures go to
// logErr.
func (nf *eventNotifyFlags) open(command string) []eventlog.Sink {
	if nf == nil {
		return nil
	}
	webhooks, alertmanagers := eventnotify.SplitURLs(nf.webhook), eventnotify.SplitURLs(nf.alertmanager)
	if len(webhooks) == 0 && len(alertmanagers) == 0 && *nf.config == "" {
		return nil
	}
	cfg, err := eventnotify.LoadConfig(*nf.config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		os.Exit(1)
	}
	set, err := eventnotify.Open(cfg, webhooks, alertmanagers, eventnotify.Options{ErrOut: logErr})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		os.Exit(1)
	}
	if set.Len() == 0 {
		fmt.Fprintf(os.Stderr, "%s: --event-notify-config needs --event-webhook or --event-alertmanager (or urls in the config)\n", command)
		os.Exit(1)
	}
	nf.set = set
	notifyClosers = append(notifyClosers, set.Close)
	return set.Sinks()
}

// notifyStatus is the delivery per destination of the sinks open() started
// (nil when none), for the daemon status (#154).
func (nf *eventNotifyFlags) notifyStatus() func() []statusreport.NotifyHealth {
	if nf == nil || nf.set == nil {
		return nil
	}
	return nf.set.Status
}

// notifyMetrics is the same for /metrics.
func (nf *eventNotifyFlags) notifyMetrics() func() []metrics.NotifyTarget {
	if nf == nil || nf.set == nil {
		return nil
	}
	return func() []metrics.NotifyTarget {
		var out []metrics.NotifyTarget
		for _, h := range nf.set.Status() {
			t := metrics.NotifyTarget{Sink: h.Sink, URL: h.URL, Delivered: h.Delivered, Failed: h.Failed,
				Rejected: h.Rejected, Dropped: h.Dropped, Pending: h.Pending, Active: h.Active,
				ConsecutiveFailures: h.ConsecutiveFailures}
			if h.LastSuccess != nil {
				t.LastSuccess = *h.LastSuccess
			}
			out = append(out, t)
		}
		return out
	}
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
			for _, line := range strings.Split(err.Error(), "\n") {
				fmt.Fprintf(logErr, "sakutei: warn: event sink: %s\n", line)
			}
		}
	}
	notifyClosers = nil
}

// exitAfterEvents flushes the notification sinks, then exits with code.
func exitAfterEvents(code int) {
	flushNotify()
	os.Exit(code)
}
