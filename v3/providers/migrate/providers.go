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

package migrate

// providers.go — provider ごとの書き直し（設計 §7.1 の表）。v2 の provider 1 つが
// 新しいコアのつなぎ方の設定の provider 1 つ（instance）と種類 1 つになる。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/internal/yaml"
	"github.com/tjst-t/sakutei/v3/providers/internal/yamlout"
)

// kind は v2 の provider 1 つの書き直し方。
type kind struct {
	v2       string // v2 の provider の名前
	instance string // つなぎ方の設定の provider の名前
	module   string // 新しいコアのモジュール
	typ      string // 種類（<instance>/<kind>）
	// convert は宣言の ID と項目を書き直し、r.name と r.out の fields を置く。
	convert func(c *conv, r *v2res, keys []string, f map[string]*yaml.Node) bool
	// settings はつなぎ方の設定の settings を作る。
	settings func(c *conv, inst *instance) *obj
}

// kinds は init で置く（書き直しの関数が kinds を引くので、宣言で置くと
// 初期化が循環する）。
var kinds []*kind

var mapping = map[string]*kind{}

func init() {
	kinds = []*kind{
		{v2: "dnszone", instance: "dns", module: "dnszone", typ: "dns/record", convert: dnsRecord, settings: dnsSettings},
		{v2: "nftables", instance: "fw", module: "nftables", typ: "fw/chain", convert: fwChain, settings: fwSettings},
		{v2: "fake", instance: "fake", module: "fake", typ: "fake/resource", convert: fakeResource, settings: fakeSettings},
		{v2: "redfish-system", instance: "bmc", module: "redfish", typ: "bmc/system", convert: passFields, settings: bmcSettings},
		{v2: "redfish-virtual-media", instance: "bmc", module: "redfish", typ: "bmc/virtual-media", convert: passFields, settings: bmcSettings},
		{v2: "template-file", instance: "answers", module: "template-file", typ: "answers/file", convert: templateFile, settings: templateSettings},
		{v2: "guest-os", instance: "guest", module: "guest-os", typ: "guest/os", convert: nil, settings: guestSettings},
		{v2: "sqlmigrate", instance: "db", module: "sqlmigrate", typ: "db/database", convert: sqlDatabase, settings: sqlSettings},
	}
	for _, k := range kinds {
		if k.convert != nil {
			mapping[k.v2] = k
		}
	}
}

func known() []string {
	var s []string
	for _, k := range kinds {
		if k.convert != nil {
			s = append(s, k.v2)
		}
	}
	return s
}

// typeOf は v2 の provider の名前の新しい種類（手順の参照の書き直しに使う）。
func typeOf(v2 string) (string, bool) {
	for _, k := range kinds {
		if k.v2 == v2 {
			return k.typ, true
		}
	}
	return "", false
}

func kindOf(v2 string) *kind {
	for _, k := range kinds {
		if k.v2 == v2 {
			return k
		}
	}
	return nil
}

// instance はつなぎ方の設定の provider 1 つ。
type instance struct {
	name   string
	module string
	kinds  []*kind
	res    []*v2res
}

func (c *conv) instance(k *kind) *instance {
	inst := c.instances[k.instance]
	if inst == nil {
		inst = &instance{name: k.instance, module: k.module}
		c.instances[k.instance] = inst
	}
	for _, x := range inst.kinds {
		if x == k {
			return inst
		}
	}
	inst.kinds = append(inst.kinds, k)
	return inst
}

// loadSettings は v2 の --provider-config と起動の引数を provider ごとの設定に
// する（ファイルの値が勝つ、v2 と同じ）。
func (c *conv) loadSettings() error {
	c.settings = map[string]map[string]any{}
	flag := func(provider, key, flagName string) {
		if v := c.in.Flags[flagName]; v != "" {
			if c.settings[provider] == nil {
				c.settings[provider] = map[string]any{}
			}
			c.settings[provider][key] = v
		}
	}
	flag("dnszone", "zone", "zone")
	flag("nftables", "ruleset", "ruleset")
	flag("nftables", "target", "ruleset-target")
	flag("fake", "state", "state")
	flag("redfish-system", "redfish_config", "redfish-config")
	flag("redfish-virtual-media", "redfish_config", "redfish-config")
	flag("sqlmigrate", "sqlmigrate_config", "sqlmigrate-config")
	if c.in.ProviderConfig == "" {
		return nil
	}
	n, err := readYAML(c.in.ProviderConfig)
	if err != nil {
		return err
	}
	for _, k := range n.Keys {
		if k != "providers" {
			return fmt.Errorf("%s: 最上位に知らない項目 %q（providers だけ）", c.in.ProviderConfig, k)
		}
	}
	ps := n.Map["providers"]
	if ps == nil || ps.Kind != yaml.Map {
		return nil
	}
	base := filepath.Dir(c.in.ProviderConfig)
	for _, name := range ps.Keys {
		m, ok := ps.Map[name].Value().(map[string]any)
		if !ok {
			return fmt.Errorf("%s: providers.%s はマップ", c.in.ProviderConfig, name)
		}
		if c.settings[name] == nil {
			c.settings[name] = map[string]any{}
		}
		for k, v := range m {
			// ファイルの中の相対パスはファイルの置き場から（v2 の --provider-config と同じ）。
			if s, ok := v.(string); ok && pathKey(k) && s != "" && !filepath.IsAbs(s) {
				v = filepath.Join(base, s)
			}
			c.settings[name][k] = v
		}
	}
	return nil
}

func pathKey(k string) bool {
	switch k {
	case "remote_config", "template_dir", "output_dir", "redfish_config", "sqlmigrate_config", "zone", "ruleset", "state":
		return true
	}
	return false
}

func readYAML(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var n *yaml.Node
	if strings.HasSuffix(path, ".json") {
		n, err = yaml.ParseJSON(data)
	} else {
		n, err = yaml.Parse(data)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if n == nil || n.Kind != yaml.Map {
		return nil, fmt.Errorf("%s: 最上位はマップ", path)
	}
	return n, nil
}

func (c *conv) setting(provider, key string) string {
	s, _ := c.settings[provider][key].(string)
	return s
}

// secretFile は v2 の秘密の参照を、中身を置いたファイルにする。
func (c *conv) secretFile(at, ref string) string {
	if p, ok := c.in.SecretFiles[ref]; ok {
		return p
	}
	c.refuse("%s: 秘密の参照 %s は新しいコアではファイルで渡す（--secret-file %s=<ファイル>）", at, ref, ref)
	return ""
}

func fields(r *v2res) *obj {
	f, ok := r.out.Get("fields").(*obj)
	if !ok {
		f = newObj()
		r.out.Set("fields", f)
	}
	return f
}

// scalarOrPool は v2 の項目の値（文字列か {from_pool: …}）。
func scalarOrPool(c *conv, r *v2res, k string, n *yaml.Node) (any, bool) {
	if n.Kind == yaml.Map {
		if n.Map["from_pool"] == nil {
			c.refuse("%s: 項目 %s のマップは from_pool だけ", r.v2id, k)
			return nil, false
		}
		// v2 の項目は文字列で、払った値を 10 進の文字列で入れた。新しいコアは
		// 整数を入れるので、文字列の項目には {n} の template を足す。
		o := value(n).(*obj)
		if o.Get("template") == nil {
			o.Set("template", "{n}")
		}
		return o, true
	}
	s, ok := str(n)
	if !ok {
		c.refuse("%s: 項目 %s は文字列", r.v2id, k)
	}
	return s, ok
}

// intOr は数の字句なら整数にする（v2 の項目は全部文字列、新しいコアは型を持つ）。
func intOr(s string) any {
	if n, err := strconv.Atoi(s); err == nil && strconv.Itoa(n) == s {
		return n
	}
	return s
}

// --- dnszone ---------------------------------------------------------------

func dnsRecord(c *conv, r *v2res, keys []string, f map[string]*yaml.Node) bool {
	r.typ, r.name = "dns/record", r.local
	fqdn, typ, ok := strings.Cut(r.local, "/")
	if !ok || !strings.HasSuffix(fqdn, ".") {
		c.refuse("%s: id は <FQDN>/<型>", r.v2id)
		return false
	}
	out := fields(r)
	for _, k := range keys {
		switch k {
		case "name", "type":
			// ID に入っている。食い違えば v2 でも断られていたはず。
			if s, _ := str(f[k]); (k == "name" && !strings.EqualFold(s, fqdn)) || (k == "type" && !strings.EqualFold(s, typ)) {
				c.refuse("%s: 項目 %s %q が id と食い違う", r.v2id, k, s)
			}
		case "value":
			v, ok := scalarOrPool(c, r, k, f[k])
			if ok {
				out.Set("value", v)
			}
		case "ttl":
			s, _ := str(f[k])
			out.Set("ttl", intOr(s))
		default:
			c.refuse("%s: dnszone の項目 %s は書き直せない", r.v2id, k)
		}
	}
	return true
}

// dnsZone は v2 のゾーンファイルのパスから、新しいコアの zone_dir とゾーンの名前を
// 出す（新しいコアはゾーン <zone> を <zone_dir>/<zone>zone から読む）。
func (c *conv) dnsZone() (dir, zone string, ok bool) {
	path := c.setting("dnszone", "zone")
	if path == "" {
		c.refuse("dnszone: ゾーンファイルが分からない（v2 の --zone を --zone で渡す）")
		return "", "", false
	}
	base := filepath.Base(path)
	zone = strings.TrimSuffix(base, "zone")
	if zone == base || !strings.HasSuffix(zone, ".") {
		c.refuse("dnszone: 新しいコアはゾーン <zone> を <zone_dir>/<zone>zone から読む。%s の名前を <ゾーン>.zone（例 example.com.zone）に変えてから書き直す", path)
		return "", "", false
	}
	return filepath.Dir(path), zone, true
}

func dnsSettings(c *conv, inst *instance) *obj {
	dir, _, ok := c.dnsZone()
	if !ok {
		return newObj()
	}
	return newObj().Set("zone_dir", dir)
}

// --- nftables --------------------------------------------------------------

func fwChain(c *conv, r *v2res, keys []string, f map[string]*yaml.Node) bool {
	parts := strings.Split(r.local, "/")
	if len(parts) != 3 {
		c.refuse("%s: id は <family>/<table>/<chain>", r.v2id)
		return false
	}
	if t := c.setting("nftables", "target"); t != "" && t != "file" {
		c.refuse("%s: nftables の %s の対象は書き直せない（file だけ。live はホストの設定を settings.hosts に書く）", r.v2id, t)
		return false
	}
	r.typ, r.name = "fw/chain", c.in.NftHost+"/"+r.local
	out := fields(r)
	for _, k := range keys {
		s, ok := str(f[k])
		if !ok {
			c.refuse("%s: 項目 %s は文字列", r.v2id, k)
			continue
		}
		switch k {
		case "family", "table", "chain":
			want := map[string]string{"family": parts[0], "table": parts[1], "chain": parts[2]}[k]
			if s != want {
				c.refuse("%s: 項目 %s %q が id と食い違う", r.v2id, k, s)
			}
		case "rules":
			rules := []any{}
			for _, x := range strings.Split(s, ";") {
				if x = strings.TrimSpace(x); x != "" {
					rules = append(rules, x)
				}
			}
			out.Set("rules", rules)
		case "type", "hook", "policy":
			out.Set(k, s)
		case "priority":
			out.Set(k, intOr(s))
		default:
			c.refuse("%s: nftables の項目 %s は書き直せない", r.v2id, k)
		}
	}
	return true
}

func fwSettings(c *conv, inst *instance) *obj {
	path := c.setting("nftables", "ruleset")
	if path == "" {
		c.refuse("nftables: ruleset のファイルが分からない（v2 の --ruleset を --ruleset で渡す）")
	}
	return newObj().Set("hosts", newObj().Set(c.in.NftHost, newObj().Set("ruleset", path)))
}

// --- fake ------------------------------------------------------------------

func fakeResource(c *conv, r *v2res, keys []string, f map[string]*yaml.Node) bool {
	r.typ, r.name = "fake/resource", r.local
	out := fields(r)
	for _, k := range keys {
		s, ok := str(f[k])
		if !ok {
			c.refuse("%s: 項目 %s は文字列", r.v2id, k)
			continue
		}
		out.Set(k, s)
	}
	return true
}

// v2fake は v2 の fake の state（{"resources": {<local>: {"fields": …}},
// "scopes": {<local>: {…}}}）。
type v2fake struct {
	Resources map[string]struct {
		Fields map[string]any `json:"fields"`
	} `json:"resources"`
	Scopes map[string]map[string]string `json:"scopes"`
}

// fakeState は v2 の fake の state を読む（無ければ空、1 回だけ読む）。
func (c *conv) fakeState() *v2fake {
	if c.fake != nil {
		return c.fake
	}
	c.fake = &v2fake{}
	path := c.setting("fake", "state")
	if path == "" {
		return c.fake
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.refuse("fake: state %s: %v", path, err)
		}
		return c.fake
	}
	if err := json.Unmarshal(data, c.fake); err != nil {
		c.refuse("fake: state %s: %v", path, err)
	}
	return c.fake
}

// fakeSettings は v2 の fake の state を新しいコアの fake の state
// （{"fake/resource/<名前>": {"availability": "present", "fields": …, "labels": …}}。
// v2 の scope は現実のラベル）に写し、種類の項目を全部文字列で宣言する（v2 の
// 項目は文字列）。
func fakeSettings(c *conv, inst *instance) *obj {
	names := map[string]bool{}
	for _, r := range inst.res {
		if r.out == nil {
			continue
		}
		if f, ok := r.out.Get("fields").(*obj); ok {
			for _, k := range f.Keys {
				names[k] = true
			}
		}
	}
	state := map[string]any{}
	v2 := c.fakeState()
	for local, res := range v2.Resources {
		fs := map[string]any{}
		for k, v := range res.Fields {
			fs[k] = fmt.Sprint(v)
			names[k] = true
		}
		e := map[string]any{"availability": "present", "fields": fs}
		if l := v2.Scopes[local]; len(l) > 0 {
			e["labels"] = l
		}
		state["fake/resource/"+local] = e
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	c.files["fake-state.json"] = append(data, '\n')
	fieldTypes := newObj()
	var ks []string
	for k := range names {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		fieldTypes.Set(k, "string")
	}
	return newObj().
		Set("state", filepath.Join(c.in.OutDir, "fake-state.json")).
		Set("kinds", newObj().Set("resource", newObj().Set("fields", fieldTypes)))
}

// fakeRanges は v2 の fake の PruneWithinScope を範囲にする。v2 は宣言の scope を
// 含む scope（ラベルが多いもの）の宣言外も消した。新しいコアの fake の範囲は
// ラベルがちょうど scope のものを選ぶので、宣言の scope と、それを含む今の現実の
// scope ごとに範囲を 1 つ書く（後から現れた scope は範囲に入らない）。
func (c *conv) fakeRanges(inst *instance) []any {
	var prune []map[string]string
	scopes := map[string]map[string]string{}
	for _, r := range inst.res {
		if r.out == nil {
			continue
		}
		scopes[fakeScope(r.scope)] = r.scope
		if r.prune {
			prune = append(prune, r.scope)
		}
	}
	if len(prune) == 0 {
		return nil
	}
	covered := func(s map[string]string) bool {
		for _, p := range prune {
			ok := true
			for k, v := range p {
				if s[k] != v {
					ok = false
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	v2 := c.fakeState()
	for local := range v2.Resources {
		if s := v2.Scopes[local]; covered(s) {
			scopes[fakeScope(s)] = s
			c.realIDs[v2ID("fake", s, local)] = "fake/resource/" + local
		}
	}
	keys := make([]string, 0, len(scopes))
	for k := range scopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []any
	for _, k := range keys {
		rg := newObj().Set("provider", "fake").Set("args", newObj().Set("scope", k))
		if len(scopes[k]) > 0 {
			rg.Set("labels", strObj(scopes[k]))
		}
		if covered(scopes[k]) {
			rg.Set("prune", true)
		}
		out = append(out, rg)
	}
	c.note("fake: v2 の PruneWithinScope を、宣言の scope とそれを含む今の現実の scope ごとの範囲（scope）にした。後から現れた scope は範囲に入らない")
	return out
}

func fakeScope(s map[string]string) string {
	parts := make([]string, 0, len(s))
	for k, v := range s {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// --- redfish ---------------------------------------------------------------

func passFields(c *conv, r *v2res, keys []string, f map[string]*yaml.Node) bool {
	r.typ, _ = typeOf(r.provider)
	r.name = r.local
	out := fields(r)
	for _, k := range keys {
		s, ok := str(f[k])
		if !ok {
			c.refuse("%s: 項目 %s は文字列", r.v2id, k)
			continue
		}
		out.Set(k, s)
	}
	return true
}

// bmcSettings は v2 の --redfish-config（bmcs.<node>.address・username・password
// …）を settings.nodes にする。パスワードは秘密の参照からファイルへ。
func bmcSettings(c *conv, inst *instance) *obj {
	path := c.setting("redfish-system", "redfish_config")
	if path == "" {
		path = c.setting("redfish-virtual-media", "redfish_config")
	}
	if path == "" {
		c.refuse("redfish: BMC の一覧が分からない（v2 の --redfish-config を --redfish-config で渡す）")
		return newObj()
	}
	n, err := readYAML(path)
	if err != nil {
		c.refuse("redfish: %v", err)
		return newObj()
	}
	nodes := newObj()
	bmcs := n.Map["bmcs"]
	if bmcs == nil || bmcs.Kind != yaml.Map {
		c.refuse("redfish: %s に bmcs が無い", path)
		return newObj()
	}
	for _, name := range bmcs.Keys {
		b := bmcs.Map[name]
		at := "redfish: " + path + ": bmcs." + name
		o := newObj()
		cred := newObj()
		for _, k := range b.Keys {
			s, _ := str(b.Map[k])
			switch k {
			case "address":
				o.Set("endpoint", s)
			case "system", "manager", "timeout":
				o.Set(k, s)
			case "ca_file":
				if !filepath.IsAbs(s) {
					s = filepath.Join(filepath.Dir(path), s)
				}
				o.Set(k, s)
			case "insecure_skip_verify":
				o.Set(k, s == "true")
			case "username":
				cred.Set("username", s)
			case "password":
				cred.Set("password_file", c.secretFile(at, s))
			default:
				c.refuse("%s.%s は書き直せない", at, k)
			}
		}
		o.Set("observe", cred)
		nodes.Set(name, o)
	}
	return newObj().Set("nodes", nodes)
}

// --- template-file ---------------------------------------------------------

func templateFile(c *conv, r *v2res, keys []string, f map[string]*yaml.Node) bool {
	r.typ, r.name = "answers/file", r.local
	out := fields(r)
	vars := newObj()
	for _, k := range keys {
		s, ok := str(f[k])
		if !ok {
			c.refuse("%s: 項目 %s は文字列", r.v2id, k)
			continue
		}
		if v, isVar := strings.CutPrefix(k, "var."); isVar {
			vars.Set(v, s)
			continue
		}
		switch k {
		case "template", "hostname", "address", "ssh_authorized_keys", "install_mark":
			out.Set(k, s)
		default:
			c.refuse("%s: template-file の項目 %s は書き直せない", r.v2id, k)
		}
	}
	if vars.Len() > 0 {
		out.Set("vars", vars)
	}
	return true
}

func templateSettings(c *conv, inst *instance) *obj {
	o := newObj()
	for _, k := range []string{"template_dir", "output_dir"} {
		v := c.setting("template-file", k)
		if v == "" {
			c.refuse("template-file: %s が分からない（v2 の --provider-config の providers.template-file）", k)
		}
		o.Set(k, v)
	}
	return o
}

// --- guest-os --------------------------------------------------------------

// guestHosts は v2 の guest_hosts と guest_markers（設定）。v2 は目印を設定に
// 書いた。新しいコアは宣言の項目 markers に書く（§7.1）。
func (c *conv) guestHosts() (hosts []string, markers *obj) {
	if c.settings["guest-os"] == nil {
		return nil, nil
	}
	for _, h := range strings.Split(c.setting("guest-os", "guest_hosts"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	markers = newObj()
	for _, m := range strings.Split(c.setting("guest-os", "guest_markers"), ",") {
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		k, v, ok := strings.Cut(m, "=")
		if !ok {
			c.refuse("guest-os: guest_markers の %q は <名前>=<パス>", m)
			continue
		}
		markers.Set(k, v)
	}
	for k := range c.settings["guest-os"] {
		switch k {
		case "guest_hosts", "guest_markers", "remote_config":
		default:
			c.refuse("guest-os: 設定の %s は書き直せない", k)
		}
	}
	return hosts, markers
}

// extraResources は v2 で設定に書いていた宣言（guest-os の目印）を宣言にする。
func (c *conv) extraResources() []any {
	hosts, markers := c.guestHosts()
	var out []any
	for _, h := range hosts {
		o := newObj().Set("type", "guest/os").Set("id", h)
		if markers.Len() > 0 {
			o.Set("fields", newObj().Set("markers", markers))
		}
		out = append(out, o)
		c.instance(kindOf("guest-os"))
	}
	return out
}

func guestSettings(c *conv, inst *instance) *obj {
	path := c.setting("guest-os", "remote_config")
	if path == "" {
		c.refuse("guest-os: remote_config（対象のホストの一覧）が分からない")
		return newObj()
	}
	n, err := readYAML(path)
	if err != nil {
		c.refuse("guest-os: %v", err)
		return newObj()
	}
	base := filepath.Dir(path)
	rel := func(s string) string {
		if s != "" && !filepath.IsAbs(s) {
			return filepath.Join(base, s)
		}
		return s
	}
	knownHosts, _ := str(n.Map["known_hosts"])
	hosts := newObj()
	hs := n.Map["hosts"]
	if hs == nil || hs.Kind != yaml.Map {
		c.refuse("guest-os: %s に hosts が無い", path)
		return newObj()
	}
	for _, name := range hs.Keys {
		h := hs.Map[name]
		at := "guest-os: " + path + ": hosts." + name
		ssh := newObj()
		for _, k := range h.Keys {
			s, _ := str(h.Map[k])
			switch k {
			case "address", "user":
				ssh.Set(k, s)
			case "port":
				ssh.Set(k, intOr(s))
			case "private_key":
				ssh.Set("key", c.secretFile(at, s))
			case "known_hosts":
				knownHosts = s
			case "connect_timeout", "timeout":
				c.note("%s.%s は捨てた（新しいコアの ssh の層の既定を使う）", at, k)
			default:
				c.refuse("%s.%s は書き直せない", at, k)
			}
		}
		if knownHosts != "" {
			ssh.Set("known_hosts", rel(knownHosts))
		}
		hosts.Set(name, newObj().Set("ssh", ssh))
	}
	for _, k := range n.Keys {
		switch k {
		case "hosts", "known_hosts":
		case "secret_store":
			// 秘密の参照は --secret-file でファイルに置き換えた。
		default:
			c.refuse("guest-os: %s の %s は書き直せない", path, k)
		}
	}
	return newObj().Set("hosts", hosts)
}

// --- sqlmigrate ------------------------------------------------------------

func sqlDatabase(c *conv, r *v2res, keys []string, f map[string]*yaml.Node) bool {
	r.typ, r.name = "db/database", r.local
	out := fields(r)
	for _, k := range keys {
		s, ok := str(f[k])
		if k != "version" || !ok {
			c.refuse("%s: sqlmigrate の項目は version だけ", r.v2id)
			continue
		}
		out.Set("version", intOr(s))
	}
	return true
}

// taskProcedure は v2 の update_policy.strategy: task（単純 Task）を手順にする
// （§7.1 の sqlmigrate：trigger: on_drift・invoke: migrate・done_when）。
func (c *conv) taskProcedure(r *v2res, file *obj) {
	up := r.node.Map["update_policy"]
	if up == nil || r.out == nil {
		return
	}
	strategy, _ := str(up.Map["strategy"])
	if strategy != "task" {
		c.refuse("%s: update_policy.strategy %q は書き直せない（task だけ）", r.v2id, strategy)
		return
	}
	args := newObj().Set("version", "{desired.version}")
	for _, k := range up.Keys {
		switch k {
		case "strategy":
		case "rollback_on_fail":
			b, _ := up.Map[k].Scalar.(bool)
			args.Set("rollback_on_fail", b)
		case "window":
			c.refuse("%s: update_policy.window は新しいコアの Policy の windows に当たるが、まだ受け皿が無い", r.v2id)
		default:
			c.refuse("%s: update_policy.%s は書き直せない", r.v2id, k)
		}
	}
	p := newObj().
		Set("name", "migrate-"+r.name).
		Set("target", newObj().Set("type", r.typ).Set("id", r.name)).
		Set("trigger", "on_drift").
		Set("drift_fields", []any{"version"}).
		Set("steps", []any{newObj().Set("name", "migrate").Set("invoke", newObj().Set("op", "migrate").Set("args", args))}).
		Set("done_when", "observe("+r.typ+"/{target}).version == desired.version")
	file.Set("procedures", appendSeq(file.Get("procedures"), p))
}

func sqlSettings(c *conv, inst *instance) *obj {
	path := c.setting("sqlmigrate", "sqlmigrate_config")
	if path == "" {
		c.refuse("sqlmigrate: データベースの一覧が分からない（v2 の --sqlmigrate-config を --sqlmigrate-config で渡す）")
		return newObj()
	}
	n, err := readYAML(path)
	if err != nil {
		c.refuse("sqlmigrate: %v", err)
		return newObj()
	}
	base := filepath.Dir(path)
	rel := func(s string) string {
		if s != "" && !filepath.IsAbs(s) {
			return filepath.Join(base, s)
		}
		return s
	}
	o := newObj()
	dbs := newObj()
	for _, k := range n.Keys {
		v := n.Map[k]
		switch k {
		case "busy_timeout":
			s, _ := str(v)
			o.Set("lock_timeout", s)
		case "sqlite3":
			s, _ := str(v)
			o.Set("sqlite3", s)
		case "databases":
			for _, name := range v.Keys {
				d := v.Map[name]
				do := newObj()
				for _, dk := range d.Keys {
					s, _ := str(d.Map[dk])
					switch dk {
					case "path", "migrations":
						do.Set(dk, rel(s))
					default:
						c.refuse("sqlmigrate: %s: databases.%s.%s は書き直せない（SQLite の path・migrations だけ）", path, name, dk)
					}
				}
				dbs.Set(name, do)
			}
		default:
			c.refuse("sqlmigrate: %s の %s は書き直せない", path, k)
		}
	}
	return o.Set("databases", dbs)
}

// --- 範囲とつなぎ方の設定 ---------------------------------------------------

// ranges は v2 で設定・起動の引数にあった管理の範囲を intent の ranges にする
// （不変条件 24）。deletion_policy: PruneWithinScope を書いた宣言があれば
// prune: true。
func (c *conv) ranges() []any {
	var out []any
	if inst := c.instances["dns"]; inst != nil {
		if _, zone, ok := c.dnsZone(); ok {
			rg := newObj().Set("provider", "dns").Set("args", newObj().Set("zone", zone))
			if anyPrune(inst) {
				rg.Set("prune", true)
			}
			out = append(out, rg)
		}
	}
	if inst := c.instances["fw"]; inst != nil {
		tables := map[string]bool{}
		for _, r := range inst.res {
			p := strings.Split(r.local, "/")
			tables[p[0]+"/"+p[1]] = true
		}
		var ts []string
		for t := range tables {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		rg := newObj().Set("provider", "fw").Set("args", newObj().Set("host", c.in.NftHost).Set("tables", strings.Join(ts, ",")))
		if anyPrune(inst) {
			rg.Set("prune", true)
		}
		out = append(out, rg)
	}
	if inst := c.instances["fake"]; inst != nil {
		out = append(out, c.fakeRanges(inst)...)
	}
	for _, name := range []string{"bmc", "answers", "guest", "db"} {
		if inst := c.instances[name]; inst != nil && anyPrune(inst) {
			c.refuse("%s: deletion_policy: PruneWithinScope は書き直せない（新しいコアの %s は範囲を持たない）", name, inst.module)
		}
	}
	return out
}

func anyPrune(inst *instance) bool {
	for _, r := range inst.res {
		if r.prune {
			return true
		}
	}
	return false
}

// config はつなぎ方の設定を書く。
func (c *conv) config() (string, error) {
	// 新しい ID の重なり（v2 では scope で分かれていたもの）は断る。
	seen := map[string]string{}
	for v2, v3 := range c.ids {
		if prev, ok := seen[v3]; ok {
			a, b := prev, v2
			if a > b {
				a, b = b, a
			}
			c.refuse("%s と %s が同じ %s になる（新しいコアの ID は scope を持たない）", a, b, v3)
		}
		seen[v3] = v2
	}
	var names []string
	for n := range c.instances {
		names = append(names, n)
	}
	sort.Strings(names)
	ps := newObj()
	for _, n := range names {
		inst := c.instances[n]
		ps.Set(n, newObj().Set("module", inst.module).Set("settings", inst.kinds[0].settings(c, inst)))
	}
	return yamlout.Emit(newObj().Set("providers", ps),
		"sakutei-migrate が v2 の provider の設定から書き直したつなぎ方の設定（範囲・方針は intent にある）。"), nil
}
