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

// Package s3 は S3 互換の API のバケットを宣言どおりに保つ provider（設計 §7.1 の
// s3-bucket、module 名 s3、E-C1・N-C7）。v2 の s3-bucket を移した。S3 互換の API
// だけを使い（SigV4 は stdlib、sigv4.go）、特定の製品に依らない。アカウント・
// 利用者・鍵の管理は持たない。コアの外のモジュールで、コアの口 core.Provider・
// core.Validator・core.Classifier だけを持つ。
//
//	providers:                                   # つなぎ方の設定
//	  s3:
//	    module: s3
//	    settings:
//	      endpoints:
//	        minio:                               # endpoint の名前（ID の前半）
//	          endpoint: https://s3.example.net:9000   # scheme は要る（http・https）
//	          api_base_path: /s3                 # 省けば前置き無し
//	          region: us-east-1                  # 省けば us-east-1
//	          addressing: path                   # path（既定）か virtual
//	          ca_file: /etc/sakutei/s3-ca.pem    # か insecure_skip_verify: true（明示したときだけ）
//	          timeout: 10s                       # 1 つの要求の時間切れ（既定 10s）
//	          fields: [versioning, policy, lifecycle, tags, object_lock]   # 扱う設定（省けば object_lock の外の 4 つ）
//	          storage_class_aliases: {VENDOR_COLD: GLACIER}              # endpoint の StorageClass の名前 → 標準の名前
//	          observe: {access_key: sakutei-ro, secret_key_file: /etc/sakutei/s3-ro.secret}
//	          write:   {access_key_file: /etc/sakutei/s3-rw.id, secret_key_file: /etc/sakutei/s3-rw.secret}  # 省けば observe
//
//	ranges:                                      # intent（扱う endpoint と接頭辞は intent に書く）
//	  - {provider: s3, args: {endpoint: minio, prefix: app-}}
//	resources:
//	  - type: s3/bucket
//	    id: minio/app-logs                       # <endpoint>/<バケット>
//	    fields:
//	      versioning: Enabled
//	      policy: {Version: "2012-10-17", Statement: [{Effect: Allow, Principal: "*", Action: "s3:GetObject", Resource: "arn:aws:s3:::app-logs/*"}]}
//	      lifecycle: [{ID: expire-logs, Status: Enabled, Filter: {Prefix: logs/}, Expiration: {Days: 30}}]
//	      tags: {env: prod}
//	      object_lock: {enable_on_create: true, mode: COMPLIANCE, years: 7}
//
// 種類は bucket（ID <endpoint>/<バケット>）だけ。項目（宣言したものだけを比べ、
// 書かないものは触らない、E-D7）：
//
//   - versioning：Enabled か Suspended。
//   - policy：バケットのポリシー（マップ）。normalize.go の正規形で比べる。{} は
//     ポリシーが無いこと（宣言すれば外す）。
//   - lifecycle：ルールの一覧（各ルールは S3 の XML の Rule をマップにした形で ID と
//     Status が要る）。正規形で比べる。[] はルールが無いこと。
//   - tags：キー → 値（文字列）のマップ。{} はタグが無いこと。
//   - object_lock：Object Lock（マップ、objectlock.go）。endpoint の fields に
//     書いたときだけ扱う。
//   - 観測だけ：creation_date（ListBuckets の CreationDate）と unsupported（endpoint
//     がその API を持たない（NotImplemented）か、endpoint の fields に無い設定の
//     集合）。宣言できない。
//
// 決まり：
//
//   - endpoint の fields に無い設定は読みに行かず（Get を呼ばない。API を持たない
//     ことを 501 でなく 400・403 で知らせる endpoint でもバケットを観測できる
//     ように）、宣言すれば読み込みで断る（E-C1・E-I7）。lifecycle の StorageClass
//     を実装独自の名前で持つ endpoint は storage_class_aliases で読み替えてから
//     比べる。
//   - 既にあるバケットの Object Lock の保持の設定を変える変更は、向きによらず
//     承認の要る壊す変更（Classify。延ばす・COMPLIANCE へ変える向きは戻せず、
//     短くする・外す向きはこれから書く版の守りが弱まる、E-C1）。作るときに掛ける
//     のはふつうの作成。外す・作り直しの要る宣言は Apply が何も書かずに断る。
//   - バケットは宣言の外で消さない（DestructiveDelete、E-C3・N-C2）。消すのは
//     ensure: absent の名指しだけで、中身のあるバケットは断る（*core.RefuseError。
//     オブジェクトを消す操作は持たない、E-F1）。
//   - 届かない・5xx・時間切れ・認証の失敗・壊れた出力は Unknown（無いと読まない、
//     不変条件 3、E-K1）。List は ListBuckets を最後まで辿れたときだけ完全（E-K2）。
//   - 書き込み用の資格（settings.endpoints.<名前>.write）は Apply・Delete の中で
//     だけ読む（E-G5）。観測・List・Delete の前の中身の確かめは observe の資格だけ。
//   - Range は args {endpoint: <名前>, prefix: <接頭辞>}（prefix は省ける）。どの
//     Range にも入らない宣言と、同じ endpoint で接頭辞の重なる Range は断る（E-I6）。
package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema の版。比べ方・正規形を変えたら上げる（E-J12）。
const Version = "s3/1"

// KindBucket は種類の名前。
const KindBucket = "bucket"

// 項目の名前。
const (
	FieldVersioning   = "versioning"
	FieldPolicy       = "policy"
	FieldLifecycle    = "lifecycle"
	FieldTags         = "tags"
	FieldObjectLock   = "object_lock"
	FieldCreationDate = "creation_date"
	FieldUnsupported  = "unsupported"
)

// versioning の値。
const (
	VersioningEnabled   = "Enabled"
	VersioningSuspended = "Suspended"
)

// SettingFields は endpoint の fields に書ける設定（Apply が合わせる順）。
// versioning を先にする（既にあるバケットに Object Lock を掛けるには versioning が
// Enabled であることを求める endpoint がある）。
var SettingFields = []string{FieldVersioning, FieldPolicy, FieldLifecycle, FieldTags, FieldObjectLock}

// DefaultFields は fields を書かない endpoint で扱う設定。object_lock は入れない
// （書いたときだけ扱う。GetObjectLockConfiguration を持たないことを 501 の外で
// 知らせる実装や、その許可の無い観測用の資格で、バケット全体が観測できなく
// ならないように）。
var DefaultFields = []string{FieldVersioning, FieldPolicy, FieldLifecycle, FieldTags}

// apiOf は設定を観測する S3 の API の名前（文言用）。
var apiOf = map[string]string{
	FieldVersioning: "GetBucketVersioning",
	FieldPolicy:     "GetBucketPolicy",
	FieldLifecycle:  "GetBucketLifecycleConfiguration",
	FieldTags:       "GetBucketTagging",
	FieldObjectLock: "GetObjectLockConfiguration",
}

// endpointName は endpoint の名前（ID の前半。"/" を含まない）。
var endpointName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// bucketName は S3 のバケット名の決まり（3〜63 文字、小文字・数字・"."・"-"、
// 先頭と末尾は英数字）。
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// prefixChars は Range の接頭辞に書ける文字。
var prefixChars = regexp.MustCompile(`^[a-z0-9.-]*$`)

// ValidBucketName は name が S3 のバケット名として使えるかを返す。
func ValidBucketName(name string) bool {
	return bucketName.MatchString(name) && !strings.Contains(name, "..") && net.ParseIP(name) == nil
}

// Endpoint は 1 つの endpoint へのつなぎ方。
type Endpoint struct {
	Name string
	// obs は観測用の資格の Client、write は書き込み用の資格の Client（Apply・
	// Delete の中でだけ Open する、E-G5）。
	obs, write *Client
	// fields は扱う設定（nil なら DefaultFields）。
	fields []string
	// aliases は endpoint が返す lifecycle の StorageClass の名前 → 標準の名前。
	aliases map[string]string
}

// handles は endpoint が field を扱うか。
func (e *Endpoint) handles(field string) bool {
	if e.fields == nil {
		return contains(DefaultFields, field)
	}
	return contains(e.fields, field)
}

// writer は書き込み用の Client（WriteContext の中でだけ使う、E-G5）。
func (e *Endpoint) writer(core.WriteContext) *Client { return e.write }

// Provider は s3 の provider。
type Provider struct {
	endpoints map[string]*Endpoint
	settle    time.Duration
}

var (
	_ core.Provider   = (*Provider)(nil)
	_ core.Validator  = (*Provider)(nil)
	_ core.Classifier = (*Provider)(nil)
)

// keys は資格の在りか。access key は書くかファイル、secret key はファイルだけ
// （値は要求のたびに読み、持たない、E-G2）。
type keys struct {
	accessKey, accessKeyFile, secretKeyFile string
}

func (k keys) credentials(context.Context) (Credentials, error) {
	read := func(name, file string) (string, error) {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("%s のファイル %s を読めない", name, file)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	ak := k.accessKey
	if k.accessKeyFile != "" {
		var err error
		if ak, err = read("access_key_file", k.accessKeyFile); err != nil {
			return Credentials{}, err
		}
	}
	sk, err := read("secret_key_file", k.secretKeyFile)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{AccessKey: ak, SecretKey: sk}, nil
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	p := &Provider{endpoints: map[string]*Endpoint{}}
	for k, v := range settings {
		switch k {
		case "endpoints":
			m, ok := v.(map[string]any)
			if !ok || len(m) == 0 {
				return nil, errors.New("settings.endpoints に endpoint を 1 つ以上書く（名前 → つなぎ方）")
			}
			for name, ev := range m {
				em, ok := ev.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("settings.endpoints.%s はマップで書く", name)
				}
				e, err := parseEndpoint(name, em)
				if err != nil {
					return nil, fmt.Errorf("settings.endpoints.%s: %w", name, err)
				}
				p.endpoints[name] = e
			}
		case "settle":
			s, _ := v.(string)
			d, err := time.ParseDuration(s)
			if err != nil || d < 0 {
				return nil, errors.New("settings.settle は長さ（5s など）")
			}
			p.settle = d
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: endpoints, settle）", k)
		}
	}
	if len(p.endpoints) == 0 {
		return nil, errors.New("settings.endpoints を書く")
	}
	return p, nil
}

func parseEndpoint(name string, m map[string]any) (*Endpoint, error) {
	if !endpointName.MatchString(name) {
		return nil, fmt.Errorf("endpoint の名前は %s に合わせる", endpointName)
	}
	for k := range m {
		switch k {
		case "endpoint", "api_base_path", "region", "addressing", "ca_file", "insecure_skip_verify", "timeout",
			"fields", "storage_class_aliases", "observe", "write":
		default:
			return nil, fmt.Errorf("知らない項目 %q（書ける項目: addressing, api_base_path, ca_file, endpoint, fields, insecure_skip_verify, observe, region, storage_class_aliases, timeout, write）", k)
		}
	}
	str := func(k string) (string, error) {
		v, ok := m[k]
		if !ok {
			return "", nil
		}
		s, isStr := v.(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("%s は文字列で書く", k)
		}
		return strings.TrimSpace(s), nil
	}
	e := &Endpoint{Name: name}
	var cfg ClientConfig
	var err error
	if cfg.Endpoint, err = str("endpoint"); err != nil {
		return nil, err
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("endpoint（http://host:port か https://host:port）を書く")
	}
	if cfg.APIBasePath, err = str("api_base_path"); err != nil {
		return nil, err
	}
	if cfg.Region, err = str("region"); err != nil {
		return nil, err
	}
	switch a, err := str("addressing"); {
	case err != nil:
		return nil, err
	case a == "" || a == "path":
	case a == "virtual":
		cfg.VirtualHost = true
	default:
		return nil, fmt.Errorf("addressing %q は path か virtual", a)
	}
	if cfg.CAFile, err = str("ca_file"); err != nil {
		return nil, err
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, errors.New("insecure_skip_verify は true か false")
		}
		cfg.InsecureSkipVerify = b
	}
	if cfg.InsecureSkipVerify && cfg.CAFile != "" {
		return nil, errors.New("ca_file と insecure_skip_verify は一緒に書けない")
	}
	if s, err := str("timeout"); err != nil {
		return nil, err
	} else if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("timeout %q は正の長さ（10s など）", s)
		}
		cfg.Timeout = d
	}
	if v, ok := m["fields"]; ok {
		arr, isArr := v.([]any)
		if !isArr {
			return nil, fmt.Errorf("fields は %s の一覧で書く", strings.Join(SettingFields, ", "))
		}
		e.fields = []string{}
		for i, x := range arr {
			f, _ := x.(string)
			if !contains(SettingFields, f) {
				return nil, fmt.Errorf("fields[%d] %v は %s のどれか", i, x, strings.Join(SettingFields, ", "))
			}
			if contains(e.fields, f) {
				return nil, fmt.Errorf("fields[%d] %q が重なる", i, f)
			}
			e.fields = append(e.fields, f)
		}
	}
	if v, ok := m["storage_class_aliases"]; ok {
		am, isMap := v.(map[string]any)
		if !isMap {
			return nil, errors.New("storage_class_aliases は endpoint の StorageClass の名前 → 標準の名前（GLACIER など）のマップで書く")
		}
		e.aliases = map[string]string{}
		for from, x := range am {
			to, _ := x.(string)
			from, to = strings.TrimSpace(from), strings.TrimSpace(to)
			if from == "" || to == "" {
				return nil, fmt.Errorf("storage_class_aliases.%s は空でない StorageClass の名前", from)
			}
			e.aliases[from] = to
		}
	}
	obs, ok, err := parseKeys("observe", m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("observe（観測用の資格 {access_key か access_key_file, secret_key_file}）を書く")
	}
	write := obs
	if w, ok, err := parseKeys("write", m); err != nil {
		return nil, err
	} else if ok {
		write = w
	}
	cfg.Credentials = obs.credentials
	if e.obs, err = NewClient(cfg); err != nil {
		return nil, err
	}
	// 書き込み用の Client も今作る（資格のファイルは読まない）。
	cfg.Credentials = write.credentials
	if e.write, err = NewClient(cfg); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	return e, nil
}

func parseKeys(k string, m map[string]any) (keys, bool, error) {
	v, ok := m[k]
	if !ok {
		return keys{}, false, nil
	}
	km, isMap := v.(map[string]any)
	if !isMap {
		return keys{}, false, fmt.Errorf("%s は {access_key か access_key_file, secret_key_file} で書く", k)
	}
	var out keys
	for ck, cv := range km {
		s, isStr := cv.(string)
		if !isStr || s == "" {
			return keys{}, false, fmt.Errorf("%s.%s は文字列で書く", k, ck)
		}
		switch ck {
		case "access_key":
			out.accessKey = s
		case "access_key_file":
			out.accessKeyFile = s
		case "secret_key_file":
			out.secretKeyFile = s
		default:
			return keys{}, false, fmt.Errorf("%s に知らない項目 %q（書ける項目: access_key, access_key_file, secret_key_file。secret key そのものは書けない）", k, ck)
		}
	}
	if (out.accessKey == "") == (out.accessKeyFile == "") {
		return keys{}, false, fmt.Errorf("%s に access_key か access_key_file のどちらか一方を書く", k)
	}
	if out.secretKeyFile == "" {
		return keys{}, false, fmt.Errorf("%s に secret_key_file を書く", k)
	}
	return out, true, nil
}

// coords は ID を endpoint とバケットに分ける。
func (p *Provider) coords(id core.ID) (*Endpoint, string, error) {
	name, bucket, ok := strings.Cut(id.Name, "/")
	if !ok || name == "" || bucket == "" {
		return nil, "", fmt.Errorf("id %q は <endpoint>/<バケット>", id.Name)
	}
	e, ok := p.endpoints[name]
	if !ok {
		return nil, "", fmt.Errorf("endpoint %q が settings.endpoints に無い（ある: %s）", name, strings.Join(p.endpointNames(), ", "))
	}
	if !ValidBucketName(bucket) {
		return nil, "", fmt.Errorf("%q はバケット名でない（a-z 0-9 . - の 3〜63 文字で、英数字で始まり英数字で終わる）", bucket)
	}
	return e, bucket, nil
}

func (p *Provider) endpointNames() []string {
	names := make([]string, 0, len(p.endpoints))
	for n := range p.endpoints {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Schema は種類 bucket。観測だけの項目も Condition から読めるよう Schema に置く
// （宣言は Validate が断る）。
func (p *Provider) Schema() core.Schema {
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		KindBucket: {Settle: p.settle, DestructiveDelete: true, Fields: map[string]core.FieldSpec{
			FieldVersioning:   {Type: core.String},
			FieldPolicy:       {Type: core.Map, Match: matchPolicy},
			FieldLifecycle:    {Type: core.List, Match: matchLifecycle},
			FieldTags:         {Type: core.Map},
			FieldObjectLock:   {Type: core.Map, Match: matchLock},
			FieldCreationDate: {Type: core.String},
			FieldUnsupported:  {Type: core.Set},
		}},
	}}
}

// matchPolicy は policy を正規形で比べる。
func matchPolicy(desired, observed any) bool {
	if observed == nil {
		return false
	}
	a, err := canonPolicyOf(desired)
	if err != nil {
		return false
	}
	b, err := canonPolicyOf(observed)
	return err == nil && a == b
}

// matchLifecycle は lifecycle を正規形で比べる。
func matchLifecycle(desired, observed any) bool {
	if observed == nil {
		return false
	}
	a, ok := rulesOf(desired)
	if !ok {
		return false
	}
	b, ok := rulesOf(observed)
	return ok && canonJSON(canonRules(a)) == canonJSON(canonRules(b))
}

// prefixRange は 1 つの範囲（endpoint と接頭辞）。
type prefixRange struct {
	endpoint, prefix string
}

// Validate は宣言を確かめる（現実に触れない、E-I7・E-M3・E-I6）。範囲は
// args {endpoint: <名前>, prefix: <接頭辞>}。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	var rs []prefixRange
	for _, rg := range ranges {
		bad := false
		for k := range rg.Args {
			if k != "endpoint" && k != "prefix" {
				rej(rg.Origin, "ranges", "s3 の範囲の引数は endpoint と prefix だけ（%q が書かれている）", k)
				bad = true
			}
		}
		name, ok := rg.Args["endpoint"]
		prefix := rg.Args["prefix"]
		switch {
		case bad:
			continue
		case !ok:
			rej(rg.Origin, "ranges", "s3 の範囲に args.endpoint を書く")
			continue
		case p.endpoints[name] == nil:
			rej(rg.Origin, "ranges", "endpoint %q が settings.endpoints に無い（ある: %s）", name, strings.Join(p.endpointNames(), ", "))
			continue
		case !prefixChars.MatchString(prefix):
			rej(rg.Origin, "ranges", "prefix %q にバケット名に使えない文字がある（a-z 0-9 . -）", prefix)
			continue
		}
		overlap := false
		for _, o := range rs {
			if o.endpoint == name && (strings.HasPrefix(prefix, o.prefix) || strings.HasPrefix(o.prefix, prefix)) {
				rej(rg.Origin, "ranges", "endpoint %s の prefix %q が prefix %q と重なる（1 つのバケットは 1 つの範囲に入れる）", name, prefix, o.prefix)
				overlap = true
			}
		}
		if !overlap {
			rs = append(rs, prefixRange{name, prefix})
		}
	}
	schema := p.Schema().Kinds[KindBucket]
	for _, r := range res {
		target := r.ID.String()
		if r.ID.Kind() != KindBucket {
			rej(r.Origin, target, "種類 %q は無い（あるのは %s だけ）", r.ID.Kind(), KindBucket)
			continue
		}
		e, bucket, err := p.coords(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		in := false
		for _, o := range rs {
			in = in || (o.endpoint == e.Name && strings.HasPrefix(bucket, o.prefix))
		}
		if !in {
			rej(r.Origin, target, "どの範囲（ranges の args.endpoint・args.prefix）にも入らない")
		}
		if r.Ensure == core.EnsureAbsent {
			continue
		}
		for _, m := range validateFields(e, r.Fields, schema) {
			rej(r.Origin, target, "%s", m)
		}
	}
	return diags
}

// validateFields は宣言の項目を確かめる。
func validateFields(e *Endpoint, f core.Fields, schema core.KindSchema) []string {
	var msgs []string
	var excluded []string
	for _, k := range sortedKeys(f) {
		v := f[k]
		if _, ok := schema.Fields[k]; !ok {
			msgs = append(msgs, fmt.Sprintf("知らない項目 %q（書ける項目: %s）", k, strings.Join(SettingFields, ", ")))
			continue
		}
		if k == FieldCreationDate || k == FieldUnsupported {
			msgs = append(msgs, fmt.Sprintf("%s は観測だけで、宣言できない", k))
			continue
		}
		if !e.handles(k) {
			excluded = append(excluded, k)
			continue
		}
		switch k {
		case FieldVersioning:
			if v != VersioningEnabled && v != VersioningSuspended {
				msgs = append(msgs, fmt.Sprintf("versioning %v は %s か %s", v, VersioningEnabled, VersioningSuspended))
			}
		case FieldPolicy:
			if _, err := canonPolicyOf(v); err != nil {
				msgs = append(msgs, err.Error())
			}
		case FieldLifecycle:
			if _, err := ParseLifecycle(v); err != nil {
				msgs = append(msgs, err.Error())
			}
		case FieldTags:
			tags, err := ParseTags(v)
			if err != nil {
				msgs = append(msgs, err.Error())
				break
			}
			if len(tags) > 50 {
				msgs = append(msgs, fmt.Sprintf("tags が %d 個（S3 は 50 個まで）", len(tags)))
			}
			for _, tk := range sortedKeys(tags) {
				if len(tk) > 128 || len(tags[tk]) > 256 {
					msgs = append(msgs, fmt.Sprintf("tags の %q のキーか値が長すぎる（キーは 128、値は 256 まで）", tk))
				}
			}
		case FieldObjectLock:
			if _, err := ParseObjectLock(v); err != nil {
				msgs = append(msgs, err.Error())
			}
		}
	}
	if len(excluded) > 0 {
		hint := ""
		if contains(excluded, FieldObjectLock) && e.fields == nil {
			hint = "。object_lock は fields に書いたときだけ扱う（endpoint の扱うほかの設定と一緒に fields に書く）"
		}
		msgs = append(msgs, fmt.Sprintf("endpoint %s は %s を扱わない（settings の endpoint の fields に無い）。宣言から外すか fields に足す%s",
			e.Name, strings.Join(excluded, ", "), hint))
	}
	return msgs
}

// Observe は ID を endpoint ごとにまとめ、ListBuckets を 1 回だけ読み、各バケットの
// 設定を読む（E-D8）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	groups := map[string][]core.ID{}
	for _, id := range ids {
		if id.Kind() != KindBucket {
			out[id] = core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("種類 %q を知らない", id.Kind())}
			continue
		}
		e, _, err := p.coords(id)
		if err != nil {
			out[id] = unknown(err)
			continue
		}
		groups[e.Name] = append(groups[e.Name], id)
	}
	for name, group := range groups {
		e := p.endpoints[name]
		sess, err := e.obs.Open(ctx)
		var buckets []BucketInfo
		if err == nil {
			buckets, _, err = sess.ListBuckets(ctx)
		}
		if err != nil {
			// 一覧が途中までしか読めなければ、無いとは言えない（E-K1・E-K2）。
			for _, id := range group {
				out[id] = unknown(err)
			}
			continue
		}
		for _, id := range group {
			_, bucket, _ := p.coords(id)
			out[id] = core.Observation{Availability: core.Absent}
			for _, b := range buckets {
				if b.Name == bucket {
					out[id] = observeBucket(ctx, sess, e, b)
					break
				}
			}
		}
	}
	return out, nil
}

// observeBucket は 1 つのバケットの設定を読む。endpoint が持たない API の設定と、
// fields に無い設定（Get を呼ばない）は Fields に入れず、unsupported に挙げる。
func observeBucket(ctx context.Context, sess *Session, e *Endpoint, b BucketInfo) core.Observation {
	f := core.Fields{FieldCreationDate: b.CreationDate}
	var unsupported []string
	// object_lock は書いたときだけ扱う設定なので、fields に無くても挙げない。
	for _, k := range DefaultFields {
		if !e.handles(k) {
			unsupported = append(unsupported, k)
		}
	}
	// skip は NotImplemented を unsupported にする。ほかの誤りは観測を Unknown にする。
	skip := func(field string, err error) (bool, error) {
		switch {
		case err == nil:
			return false, nil
		case KindOf(err) == KindNotImplemented:
			unsupported = append(unsupported, field)
			return true, nil
		case CodeOf(err) == "NoSuchBucket":
			return true, &Error{Kind: KindUnreachable, Op: apiOf[field], Bucket: b.Name, Message: "読む間にバケットが消えた", Err: err}
		}
		return true, err
	}
	for _, k := range SettingFields {
		if !e.handles(k) {
			continue
		}
		var v any
		var err error
		switch k {
		case FieldVersioning:
			v, err = sess.GetVersioning(ctx, b.Name)
		case FieldPolicy:
			var raw string
			if raw, err = sess.GetPolicy(ctx, b.Name); err == nil {
				if v, err = policyValue(raw); err != nil {
					err = unreadable("GetBucketPolicy", b.Name, err)
				}
			}
		case FieldLifecycle:
			v, err = readLifecycle(ctx, sess, e, b.Name)
		case FieldTags:
			var tags map[string]string
			if tags, err = sess.GetTagging(ctx, b.Name); err == nil {
				v = tagsValue(tags)
			}
		case FieldObjectLock:
			var ol ObjectLock
			if ol, err = sess.GetObjectLock(ctx, b.Name); err == nil {
				v = lockFromObserved(ol).value()
			}
		}
		if s, err := skip(k, err); err != nil {
			return unknown(err)
		} else if !s {
			f[k] = v
		}
	}
	sort.Strings(unsupported)
	set := make([]any, len(unsupported))
	for i, u := range unsupported {
		set[i] = u
	}
	f[FieldUnsupported] = set
	return core.Observation{Availability: core.Present, Fields: f}
}

// readLifecycle はライフサイクルを読み、StorageClass を読み替えた正規形にする。
func readLifecycle(ctx context.Context, sess *Session, e *Endpoint, bucket string) ([]any, error) {
	data, err := sess.GetLifecycle(ctx, bucket)
	if err != nil {
		return nil, err
	}
	rules, err := lifecycleFromXML(data)
	if err != nil {
		return nil, unreadable("GetBucketLifecycleConfiguration", bucket, err)
	}
	aliasStorageClass(rules, e.aliases)
	return canonRules(rules), nil
}

// List は範囲の endpoint の、接頭辞に合うバケットを返す。ListBuckets を最後まで
// 辿れたときだけ完全（E-K2）。途中で切れたら、それまでのバケットと誤りを返す。
func (p *Provider) List(ctx context.Context, rg core.Range) (core.Listing, error) {
	name, prefix := rg.Args["endpoint"], rg.Args["prefix"]
	e, ok := p.endpoints[name]
	if !ok {
		return core.Listing{}, fmt.Errorf("endpoint %q が settings.endpoints に無い", name)
	}
	sess, err := e.obs.Open(ctx)
	if err != nil {
		return core.Listing{}, err
	}
	buckets, complete, err := sess.ListBuckets(ctx)
	var ids []core.ID
	for _, b := range buckets {
		if strings.HasPrefix(b.Name, prefix) {
			ids = append(ids, core.ID{Type: rg.Provider + "/" + KindBucket, Name: name + "/" + b.Name})
		}
	}
	if err != nil || !complete {
		if err == nil {
			err = errors.New("ListBuckets が途中で切れた")
		}
		return core.Listing{IDs: ids, Complete: false}, fmt.Errorf("一覧は完全でない: %w", err)
	}
	return core.Listing{IDs: ids, Complete: true}, nil
}

// Classify は既にあるバケットの Object Lock の保持の設定を変える変更を、向きに
// よらず承認の要る壊す変更とする（E-C1）。作るとき・変わらないとき・宣言どおりに
// できない（Apply が断る）ときは何も言わない。現実に触れない。
func (p *Provider) Classify(id core.ID, current core.Observation, desired core.Fields) []core.Hint {
	if current.Availability != core.Present {
		return nil
	}
	v, ok := desired[FieldObjectLock]
	if !ok {
		return nil
	}
	ov, ok := current.Fields[FieldObjectLock]
	if !ok {
		return nil
	}
	to, err := ParseObjectLock(v)
	if err != nil {
		return nil
	}
	from, ok := lockFromValue(ov)
	if !ok {
		return []core.Hint{{Field: FieldObjectLock, Destructive: true, NeedsApproval: true, Reason: "object_lock: 既にあるバケットの Object Lock の設定を変える"}}
	}
	if sameLock(from, to) || lockConflict(from, to) != "" {
		return nil
	}
	return []core.Hint{{Field: FieldObjectLock, Destructive: true, NeedsApproval: true, Reason: "object_lock: " + lockChange(from, to)}}
}

// Apply は宣言の設定をそろえる。無ければ作り、設定ごとに今の値を（書き込み用の
// 資格で）読み直し、ずれているものだけを置き、読み戻して確かめる（冪等）。
func (p *Provider) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, current core.Observation) error {
	e, bucket, err := p.coords(id)
	if err != nil {
		return err
	}
	for _, k := range sortedKeys(desired) {
		if contains(SettingFields, k) && !e.handles(k) {
			return fmt.Errorf("endpoint %s は %s を扱わない（endpoint の fields に無い）", e.Name, k)
		}
		if !contains(SettingFields, k) {
			return fmt.Errorf("%s は宣言できない", k)
		}
	}
	var lock LockSpec
	if v, ok := desired[FieldObjectLock]; ok {
		if lock, err = ParseObjectLock(v); err != nil {
			return err
		}
	}
	sess, err := e.writer(w).Open(ctx)
	if err != nil {
		return err
	}
	if current.Availability == core.Absent {
		if err := sess.CreateBucket(ctx, bucket, lock.EnableOnCreate); err != nil {
			if CodeOf(err) == "BucketAlreadyExists" {
				return fmt.Errorf("バケット名 %s はほかのアカウントが使っている: %w", bucket, err)
			}
			return err
		}
	}
	for _, k := range SettingFields {
		v, ok := desired[k]
		if !ok {
			continue
		}
		if err := applyField(ctx, sess, e, bucket, k, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// applyField は 1 つの設定を宣言 v にする。今の値が同じなら何も送らない。
func applyField(ctx context.Context, sess *Session, e *Endpoint, bucket, field string, v any) error {
	switch field {
	case FieldVersioning:
		got, err := sess.GetVersioning(ctx, bucket)
		if err != nil {
			return putErr(err)
		}
		if got == v {
			return nil
		}
		s, _ := v.(string)
		if err := sess.PutVersioning(ctx, bucket, s); err != nil {
			return putErr(err)
		}
		got, err = sess.GetVersioning(ctx, bucket)
		if err != nil {
			return err
		}
		return readBack(field, got == s, got)
	case FieldPolicy:
		want, err := canonPolicyOf(v)
		if err != nil {
			return err
		}
		read := func() (string, error) {
			raw, err := sess.GetPolicy(ctx, bucket)
			if err != nil {
				return "", err
			}
			c, err := CanonPolicy(raw)
			if err != nil {
				return strings.TrimSpace(raw), nil // 読めないものはそのまま（宣言の正規形とは合わない）
			}
			return c, nil
		}
		got, err := read()
		if err != nil {
			return putErr(err)
		}
		if got == want {
			return nil
		}
		if want == "" {
			err = sess.DeletePolicy(ctx, bucket)
		} else {
			body, _ := json.Marshal(v) // 宣言の書き方のまま送る（比べるのは正規形）
			err = sess.PutPolicy(ctx, bucket, string(body))
		}
		if err != nil {
			return putErr(err)
		}
		if got, err = read(); err != nil {
			return err
		}
		return readBack(field, got == want, got)
	case FieldLifecycle:
		rules, err := ParseLifecycle(v)
		if err != nil {
			return err
		}
		want := canonJSON(canonRules(rules))
		got, err := readLifecycle(ctx, sess, e, bucket)
		if err != nil {
			return putErr(err)
		}
		if canonJSON(got) == want {
			return nil
		}
		if len(rules) == 0 {
			err = sess.DeleteLifecycle(ctx, bucket)
		} else {
			err = sess.PutLifecycle(ctx, bucket, lifecycleXML(rules))
		}
		if err != nil {
			return putErr(err)
		}
		if got, err = readLifecycle(ctx, sess, e, bucket); err != nil {
			return err
		}
		hint := "（endpoint が StorageClass の名前を書き換えて持ち、PUT では標準の名前だけを受けるなら、endpoint の storage_class_aliases で読み替える）"
		if canonJSON(got) != want {
			return fmt.Errorf("endpoint は lifecycle を受けたが別の形で持つので、宣言どおりにならない。endpoint の持つ形で書く%s: %s", hint, canonJSON(got))
		}
		return nil
	case FieldTags:
		tags, err := ParseTags(v)
		if err != nil {
			return err
		}
		want := canonJSON(tags)
		cur, err := sess.GetTagging(ctx, bucket)
		if err != nil {
			return putErr(err)
		}
		if canonJSON(cur) == want {
			return nil
		}
		if len(tags) == 0 {
			err = sess.DeleteTagging(ctx, bucket)
		} else {
			err = sess.PutTagging(ctx, bucket, tags)
		}
		if err != nil {
			return putErr(err)
		}
		if cur, err = sess.GetTagging(ctx, bucket); err != nil {
			return err
		}
		return readBack(field, canonJSON(cur) == want, canonJSON(cur))
	case FieldObjectLock:
		return applyObjectLock(ctx, sess, bucket, v)
	}
	return fmt.Errorf("知らない項目 %q", field)
}

// applyObjectLock は object_lock を宣言 v にする。今の設定を読み、正規形が同じなら
// 何も送らない（作るときの有効化で済んだときなど）。外す・作り直しの要る宣言は
// 何も書かずに断る。違えば PutObjectLockConfiguration だけで掛け（versioning は
// 触らない）、読み戻して確かめる。
func applyObjectLock(ctx context.Context, sess *Session, bucket string, v any) error {
	want, err := ParseObjectLock(v)
	if err != nil {
		return err
	}
	cur, err := sess.GetObjectLock(ctx, bucket)
	if err != nil {
		return putErr(err)
	}
	from := lockFromObserved(cur)
	if sameLock(from, want) {
		return nil
	}
	if c := lockConflict(from, want); c != "" {
		return errors.New(c)
	}
	if err := lockPutErr(sess.PutObjectLock(ctx, bucket, ObjectLock{Enabled: true, Mode: want.Mode, Days: want.Days, Years: want.Years})); err != nil {
		return err
	}
	got, err := sess.GetObjectLock(ctx, bucket)
	if err != nil {
		return err
	}
	return readBack(FieldObjectLock, sameLock(lockFromObserved(got), want), canonJSON(lockFromObserved(got).value()))
}

// readBack は置いたあと読み戻した値が宣言どおりでなければ誤りにする（永久の
// ずれで Apply を繰り返さないよう、endpoint の持つ形を添える）。
func readBack(field string, ok bool, got string) error {
	if ok {
		return nil
	}
	return fmt.Errorf("endpoint は %s を受けたが別の形で持つので、宣言どおりにならない。endpoint の持つ形で書く: %s", field, got)
}

// putErr は endpoint がその API を持たないことを分かる文にする。
func putErr(err error) error {
	if KindOf(err) == KindNotImplemented {
		return fmt.Errorf("endpoint がこの API を持たない（NotImplemented）ので、この項目はここでは扱えない（宣言から外す）: %w", err)
	}
	return err
}

// Delete は ensure: absent のバケットを消す。オブジェクトがあれば何も書かずに
// 断る（観測用の資格で先に確かめる。古い版だけが残るバケットは endpoint の
// BucketNotEmpty で断る、E-F1・E-C3）。
func (p *Provider) Delete(ctx context.Context, w core.WriteContext, id core.ID, _ core.Observation) error {
	e, bucket, err := p.coords(id)
	if err != nil {
		return err
	}
	refuse := &core.RefuseError{Reason: fmt.Sprintf("バケット %s にオブジェクトがある。Sakutei はオブジェクトを消さない（手で空にしてから消す。残すなら宣言し直す）", bucket)}
	obs, err := e.obs.Open(ctx)
	if err != nil {
		return err
	}
	full, err := obs.HasObjects(ctx, bucket)
	switch {
	case CodeOf(err) == "NoSuchBucket":
		return nil
	case err != nil:
		return fmt.Errorf("中身があるかを確かめられないので消さない: %w", err)
	case full:
		return refuse
	}
	sess, err := e.writer(w).Open(ctx)
	if err != nil {
		return err
	}
	if err := sess.DeleteBucket(ctx, bucket); err != nil {
		if CodeOf(err) == "BucketNotEmpty" {
			return refuse
		}
		return err
	}
	return nil
}

// unknown は誤りの観測（届かない・認証の失敗・壊れた出力、E-K1）。
func unknown(err error) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: err.Error()}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
