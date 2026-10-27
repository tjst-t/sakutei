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

package netconf

// netconf_test.go — 区切りと正規形（Backlog #141）。
//
// Tags:
//   [AC-141-1-1] 区切り: 1.0 の "]]>]]>" と 1.1 の chunked を読み書きでき、壊れた区切りは error
//   [AC-141-1-2] 正規形: 名前空間の接頭辞・空白・属性の順・兄弟の順（既定）はずれにしない
//   [AC-141-1-3] ordered で名指しした列だけ順をずれにする

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFramer_RoundTrip(t *testing.T) {
	// [AC-141-1-1]
	for _, fr := range []Framing{FramingEOM, FramingChunked} {
		var buf bytes.Buffer
		w := NewFramer(nil, &buf)
		w.SetFraming(fr)
		msgs := []string{"<a/>", "<b>x</b>", strings.Repeat("y", 70000)}
		for _, m := range msgs {
			if err := w.WriteMessage([]byte(m)); err != nil {
				t.Fatal(err)
			}
		}
		r := NewFramer(&buf, nil)
		r.SetFraming(fr)
		for _, m := range msgs {
			got, err := r.ReadMessage()
			if err != nil || string(got) != m {
				t.Fatalf("[AC-141-1-1] %s: got %.20q, %v", fr, got, err)
			}
		}
		if _, err := r.ReadMessage(); !errors.Is(err, io.EOF) {
			t.Fatalf("[AC-141-1-1] %s: at the end err = %v, want EOF", fr, err)
		}
	}
}

func TestFramer_ChunkedSeveralChunks(t *testing.T) {
	// [AC-141-1-1] RFC 6242 §4.2 の例: 1 つのメッセージが 2 つの chunk に分かれる。
	in := "\n#4\n<rpc\n#18\n message-id=\"102\"\n\n##\n"
	r := NewFramer(strings.NewReader(in), nil)
	r.SetFraming(FramingChunked)
	got, err := r.ReadMessage()
	if err != nil || string(got) != "<rpc message-id=\"102\"\n" {
		t.Fatalf("[AC-141-1-1] got %q, %v", got, err)
	}
}

func TestFramer_Malformed(t *testing.T) {
	// [AC-141-1-1]
	for name, c := range map[string]struct {
		in string
		fr Framing
	}{
		"chunked: no header":     {"<rpc/>", FramingChunked},
		"chunked: zero size":     {"\n#0\n\n##\n", FramingChunked},
		"chunked: leading zero":  {"\n#04\n<a/>\n##\n", FramingChunked},
		"chunked: short chunk":   {"\n#10\n<a/>", FramingChunked},
		"chunked: no end":        {"\n#4\n<a/>", FramingChunked},
		"chunked: empty message": {"\n##\n", FramingChunked},
		"eom: cut":               {"<rpc-reply>", FramingEOM},
	} {
		r := NewFramer(strings.NewReader(c.in), nil)
		r.SetFraming(c.fr)
		if _, err := r.ReadMessage(); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("[AC-141-1-1] %s: err = %v, want a framing error", name, err)
		}
	}
}

func canon(t *testing.T, x, ordered string) string {
	t.Helper()
	c, err := Canonical(x, true, ordered)
	if err != nil {
		t.Fatalf("Canonical(%q): %v", x, err)
	}
	return c
}

func TestCanonical_Equivalent(t *testing.T) {
	// [AC-141-1-2]
	const ns = "urn:example:if"
	base := `<interfaces xmlns="` + ns + `"><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`
	for name, x := range map[string]string{
		"prefixed element":    `<if:interfaces xmlns:if="` + ns + `"><if:interface><if:name>eth0</if:name><if:type xmlns:other="urn:example:types">other:ethernet</if:type><if:mtu>1500</if:mtu></if:interface><if:interface><if:name>eth1</if:name></if:interface></if:interfaces>`,
		"whitespace":          "<interfaces xmlns=\"" + ns + "\">\n  <interface>\n    <name> eth0 </name>\n    <type xmlns:t=\"urn:example:types\">t:ethernet</type>\n    <mtu>1500</mtu>\n  </interface>\n  <interface><name>eth1</name></interface>\n</interfaces>\n",
		"sibling order":       `<interfaces xmlns="` + ns + `"><interface><name>eth1</name></interface><interface><mtu>1500</mtu><type xmlns:t="urn:example:types">t:ethernet</type><name>eth0</name></interface></interfaces>`,
		"comment and decl":    `<?xml version="1.0"?><!-- c --><interfaces xmlns="` + ns + `"><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
		"prefix on the root":  `<interfaces xmlns="` + ns + `" xmlns:t="urn:example:types"><interface><name>eth0</name><type>t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
		"empty element forms": `<interfaces xmlns="` + ns + `"><interface><name>eth1</name></interface><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>1500</mtu></interface></interfaces>`,
	} {
		if got, want := canon(t, x, ""), canon(t, base, ""); got != want {
			t.Errorf("[AC-141-1-2] %s:\n got %s\nwant %s", name, got, want)
		}
	}
	// Attributes: order and prefixes do not matter, values do.
	a := canon(t, `<x xmlns="urn:a" xmlns:p="urn:p" b="2" a="1" p:c="3"/>`, "")
	b := canon(t, `<x xmlns="urn:a" xmlns:q="urn:p" q:c="3" a="1" b="2"></x>`, "")
	if a != b {
		t.Errorf("[AC-141-1-2] attributes: %s != %s", a, b)
	}
	if again := canon(t, a, ""); again != a {
		t.Errorf("[AC-141-1-2] the canonical form is not stable: %s → %s", a, again)
	}

	for name, x := range map[string]string{
		"value":          `<interfaces xmlns="` + ns + `"><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>9000</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
		"namespace":      `<interfaces xmlns="urn:example:other"><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
		"identity":       `<interfaces xmlns="` + ns + `"><interface><name>eth0</name><type xmlns:t="urn:example:othertypes">t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
		"missing leaf":   `<interfaces xmlns="` + ns + `"><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type></interface><interface><name>eth1</name></interface></interfaces>`,
		"extra element":  `<interfaces xmlns="` + ns + `"><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface><interface><name>eth2</name></interface></interfaces>`,
		"attribute":      `<interfaces xmlns="` + ns + `" a="1"><interface><name>eth0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
		"inner space":    `<interfaces xmlns="` + ns + `"><interface><name>eth 0</name><type xmlns:t="urn:example:types">t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
		"undeclared pfx": `<interfaces xmlns="` + ns + `"><interface><name>eth0</name><type>t:ethernet</type><mtu>1500</mtu></interface><interface><name>eth1</name></interface></interfaces>`,
	} {
		if canon(t, x, "") == canon(t, base, "") {
			t.Errorf("[AC-141-1-2] %s: must differ", name)
		}
	}
}

func TestCanonical_Ordered(t *testing.T) {
	// [AC-141-1-3]
	a := `<acl xmlns="urn:example:acl"><name>mgmt</name><aces><ace><seq>permit ssh</seq></ace><ace><seq>deny any</seq></ace></aces></acl>`
	b := `<acl xmlns="urn:example:acl"><aces><ace><seq>deny any</seq></ace><ace><seq>permit ssh</seq></ace></aces><name>mgmt</name></acl>`
	if canon(t, a, "") != canon(t, b, "") {
		t.Errorf("[AC-141-1-3] by default the order of siblings is not a drift")
	}
	if canon(t, a, "acl/aces/ace") == canon(t, b, "acl/aces/ace") {
		t.Errorf("[AC-141-1-3] the order of an ordered list must be a drift")
	}
	// Only the named list keeps its order: the other siblings are still sorted.
	c := `<acl xmlns="urn:example:acl"><name>mgmt</name><aces><ace><seq>permit ssh</seq></ace><ace><seq>deny any</seq></ace></aces></acl>`
	d := `<acl xmlns="urn:example:acl"><aces><ace><seq>permit ssh</seq></ace><ace><seq>deny any</seq></ace></aces><name>mgmt</name></acl>`
	if canon(t, c, "acl/aces/ace") != canon(t, d, "acl/aces/ace") {
		t.Errorf("[AC-141-1-3] siblings outside the ordered list are compared as a set")
	}
	for _, bad := range []string{"acl//ace", "a:b/c", "acl/ace s"} {
		if _, err := ParseOrdered(bad); err == nil {
			t.Errorf("[AC-141-1-3] ordered %q must be an error", bad)
		}
	}
}

func TestCanonical_Malformed(t *testing.T) {
	for _, x := range []string{`<a>`, `<a></b>`, `text<a/>`, `<a xmlns:p="u"><p:b></a>`} {
		if _, err := Canonical(x, true, ""); err == nil {
			t.Errorf("Canonical(%q) must be an error", x)
		}
	}
	deep := strings.Repeat("<a>", maxDepth+1) + strings.Repeat("</a>", maxDepth+1)
	if _, err := Canonical(deep, true, ""); err == nil {
		t.Errorf("a too deep XML must be an error")
	}
}
