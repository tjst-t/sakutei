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

// Package metrics writes the convergence state of the daemon in the Prometheus
// text exposition format (version 0.0.4) with the standard library only
// (Backlog #54, §27.5).  It exports values; time series, alerting and
// notification routing stay with Prometheus / Alertmanager (§27.6).
package metrics

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ContentType is the Content-Type of the text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Type is a metric family's TYPE.
type Type string

const (
	Gauge   Type = "gauge"
	Counter Type = "counter"
)

// Label is one label pair of a sample.
type Label struct {
	Name, Value string
}

// Sample is one line of a family: its labels and value.
type Sample struct {
	Labels []Label
	Value  float64
}

// Family is a metric family: HELP, TYPE and its samples.  A family with no
// sample is not written (no data is not zero: unknown ≠ absent, §3).
type Family struct {
	Name    string
	Help    string
	Type    Type
	Samples []Sample
}

// Write writes fams in the text exposition format, in the given order.
func Write(w io.Writer, fams []Family) error {
	var b strings.Builder
	for _, f := range fams {
		if len(f.Samples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n", f.Name, escapeHelp(f.Help))
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.Name, f.Type)
		for _, s := range f.Samples {
			b.WriteString(f.Name)
			if len(s.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range s.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "%s=\"%s\"", l.Name, escapeLabelValue(l.Value))
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(formatValue(s.Value))
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// formatValue formats a sample value: Go float syntax, with the format's
// spellings of the infinities and NaN.
func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeHelp escapes a HELP text: backslash and line feed.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// escapeLabelValue escapes a label value: backslash, double quote and line feed.
func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
