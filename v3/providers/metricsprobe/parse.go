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

package metricsprobe

// parse.go は /metrics（Prometheus の text 形式）を読む（v2 の internal/metrics から
// 移した）。

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Parsed is one sample read back by ParseScrape.
type Parsed struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Key is the series of the sample: its name and sorted labels, e.g.
// sakutei_resources{state="drifted"}.
func (p Parsed) Key() string {
	if len(p.Labels) == 0 {
		return p.Name
	}
	names := make([]string, 0, len(p.Labels))
	for n := range p.Labels {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s=%q", n, p.Labels[n])
	}
	return p.Name + "{" + strings.Join(parts, ",") + "}"
}

// ParseScrape reads the text exposition format as other exporters write it:
// comments, HELP and TYPE lines
// are skipped and a sample needs no TYPE before it, so histogram and summary
// samples, untyped samples and families out of order are read.  The samples
// themselves are still checked: names, labels (spaces around the pairs are
// allowed), escapes, the value and an optional timestamp (an integer, or the
// OpenMetrics float).  A malformed line or a series twice is an error — a
// broken page is not read as a partial one.
func ParseScrape(r io.Reader) ([]Parsed, error) {
	var out []Parsed
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), " \t\r")
		line = strings.TrimLeft(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := parseSample(line, true)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		if seen[p.Key()] {
			return nil, fmt.Errorf("line %d: series %s twice", n, p.Key())
		}
		seen[p.Key()] = true
		out = append(out, p)
	}
	return out, sc.Err()
}

// parseSample parses name{labels} value [timestamp].  lenient (ParseScrape)
// allows spaces around the label pairs and their "=", runs of spaces or tabs before the
// value and the timestamp, and a float timestamp.
func parseSample(line string, lenient bool) (Parsed, error) {
	p := Parsed{Labels: map[string]string{}}
	stops := "{ "
	if lenient {
		stops = "{ \t"
	}
	i := strings.IndexAny(line, stops)
	if i < 0 {
		return p, fmt.Errorf("no value")
	}
	p.Name = line[:i]
	if !validMetricName(p.Name) {
		return p, fmt.Errorf("invalid metric name %q", p.Name)
	}
	rest := line[i:]
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			if lenient {
				rest = strings.TrimLeft(rest, " \t")
			}
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			var name string
			if lenient {
				// name = "value": spaces around the "=" are allowed.
				eq := strings.IndexByte(rest, '=')
				if eq < 0 {
					return p, fmt.Errorf("bad label")
				}
				name = strings.TrimRight(rest[:eq], " \t")
				rest = strings.TrimLeft(rest[eq+1:], " \t")
				if !strings.HasPrefix(rest, "\"") {
					return p, fmt.Errorf("bad label %s: the value is not quoted", name)
				}
				rest = rest[1:]
			} else {
				eq := strings.Index(rest, "=\"")
				if eq < 0 {
					return p, fmt.Errorf("bad label")
				}
				name = rest[:eq]
				rest = rest[eq+2:]
			}
			if !validLabelName(name) {
				return p, fmt.Errorf("invalid label name %q", name)
			}
			var v strings.Builder
			for {
				if rest == "" {
					return p, fmt.Errorf("unterminated label value")
				}
				c := rest[0]
				rest = rest[1:]
				if c == '"' {
					break
				}
				if c == '\\' {
					if rest == "" {
						return p, fmt.Errorf("bad escape")
					}
					switch rest[0] {
					case '\\':
						v.WriteByte('\\')
					case '"':
						v.WriteByte('"')
					case 'n':
						v.WriteByte('\n')
					default:
						return p, fmt.Errorf("bad escape \\%c", rest[0])
					}
					rest = rest[1:]
					continue
				}
				v.WriteByte(c)
			}
			if _, dup := p.Labels[name]; dup {
				return p, fmt.Errorf("label %s twice", name)
			}
			p.Labels[name] = v.String()
			if lenient {
				rest = strings.TrimLeft(rest, " \t")
				if !strings.HasPrefix(rest, ",") && !strings.HasPrefix(rest, "}") {
					return p, fmt.Errorf("want , or } after the label %s", name)
				}
			}
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
			}
		}
	}
	if lenient {
		return p, lenientValue(&p, rest)
	}
	if !strings.HasPrefix(rest, " ") {
		return p, fmt.Errorf("no space before the value")
	}
	f := strings.Split(rest[1:], " ")
	if len(f) < 1 || len(f) > 2 {
		return p, fmt.Errorf("want a value and an optional timestamp")
	}
	v, err := parseValue(f[0])
	if err != nil {
		return p, err
	}
	p.Value = v
	if len(f) == 2 {
		if _, err := strconv.ParseInt(f[1], 10, 64); err != nil {
			return p, fmt.Errorf("bad timestamp %q", f[1])
		}
	}
	return p, nil
}

// lenientValue reads " value [timestamp]" (ParseScrape).
func lenientValue(p *Parsed, rest string) error {
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return fmt.Errorf("no space before the value")
	}
	f := strings.Fields(rest)
	if len(f) < 1 || len(f) > 2 {
		return fmt.Errorf("want a value and an optional timestamp")
	}
	v, err := parseValue(f[0])
	if err != nil {
		return err
	}
	p.Value = v
	if len(f) == 2 {
		if _, err := strconv.ParseFloat(f[1], 64); err != nil || strings.ContainsAny(f[1], "xXpP_") {
			return fmt.Errorf("bad timestamp %q", f[1])
		}
	}
	return nil
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf", "-Inf", "NaN":
		return strconv.ParseFloat(s, 64)
	}
	if strings.ContainsAny(s, "xXpP_") || strings.EqualFold(s, "inf") || strings.EqualFold(s, "nan") {
		return 0, fmt.Errorf("bad value %q", s)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return v, nil
}

func validMetricName(s string) bool {
	for i, c := range s {
		if !(c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return s != ""
}

func validLabelName(s string) bool {
	for i, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return s != "" && !strings.HasPrefix(s, "__")
}
