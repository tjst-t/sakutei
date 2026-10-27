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

package metrics

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Parsed is one sample read back by Parse.
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

// Parse reads the text exposition format strictly, as the exporter must
// write it: every sample belongs to a family declared by one TYPE line before
// it, a family's lines are contiguous, at most one HELP and one TYPE per
// family, valid metric and label names, escaped label values, and no series
// twice.  It is the format check of the tests (and of anyone scraping by hand).
func Parse(r io.Reader) ([]Parsed, error) {
	var out []Parsed
	typed := map[string]string{}
	helped := map[string]bool{}
	closed := map[string]bool{}
	seen := map[string]bool{}
	current := ""
	enter := func(name string) error {
		if name == current {
			return nil
		}
		if closed[name] {
			return fmt.Errorf("family %s is not contiguous", name)
		}
		if current != "" {
			closed[current] = true
		}
		current = name
		return nil
	}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		fail := func(format string, a ...any) error {
			return fmt.Errorf("line %d %q: %s", n, line, fmt.Sprintf(format, a...))
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			f := strings.SplitN(line, " ", 4)
			if len(f) < 3 || f[0] != "#" || (f[1] != "HELP" && f[1] != "TYPE") {
				continue // a plain comment
			}
			name := f[2]
			if !validMetricName(name) {
				return nil, fail("invalid metric name")
			}
			if err := enter(name); err != nil {
				return nil, fail("%v", err)
			}
			if f[1] == "HELP" {
				if helped[name] {
					return nil, fail("second HELP")
				}
				helped[name] = true
				continue
			}
			if _, ok := typed[name]; ok {
				return nil, fail("second TYPE")
			}
			if len(f) != 4 {
				return nil, fail("TYPE without a type")
			}
			switch f[3] {
			case "counter", "gauge", "histogram", "summary", "untyped":
			default:
				return nil, fail("unknown type %q", f[3])
			}
			typed[name] = f[3]
			continue
		}
		p, err := parseSample(line)
		if err != nil {
			return nil, fail("%v", err)
		}
		if _, ok := typed[p.Name]; !ok {
			return nil, fail("sample before its TYPE")
		}
		if err := enter(p.Name); err != nil {
			return nil, fail("%v", err)
		}
		if typed[p.Name] == "counter" && !strings.HasSuffix(p.Name, "_total") {
			return nil, fail("counter name without _total")
		}
		if seen[p.Key()] {
			return nil, fail("series %s twice", p.Key())
		}
		seen[p.Key()] = true
		out = append(out, p)
	}
	return out, sc.Err()
}

// parseSample parses name{labels} value [timestamp].
func parseSample(line string) (Parsed, error) {
	p := Parsed{Labels: map[string]string{}}
	i := strings.IndexAny(line, "{ ")
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
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			eq := strings.Index(rest, "=\"")
			if eq < 0 {
				return p, fmt.Errorf("bad label")
			}
			name := rest[:eq]
			if !validLabelName(name) {
				return p, fmt.Errorf("invalid label name %q", name)
			}
			rest = rest[eq+2:]
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
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
			}
		}
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
