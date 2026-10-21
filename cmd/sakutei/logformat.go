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

package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tjst-t/sakutei/internal/eventlog"
	"github.com/tjst-t/sakutei/pkg/core"
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
