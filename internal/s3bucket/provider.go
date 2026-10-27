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

// Package s3bucket は S3 互換の API のバケットを収束させる provider
// （s3-bucket、Backlog #122、設計書 §29.1「S3 互換のバケット provider」）。
// S3 互換の API だけを使い、特定の製品に依らない。アカウント・利用者の管理は
// 製品ごとに違うので持たない（要るなら外の provider で足す、§28.9）。
//
// 完全の階級：Read・Apply・List・BulkObserve（ListBuckets と各バケットの設定の
// 読み。Read と等価。止まった endpoint のバケットは Read に回す）。
//
// ID は "<endpoint の名前>/<bucket>"（endpoint は設定ファイルの名前付きの一覧、
// config.go）。
//
// 宣言するフィールド（宣言したものだけを管理し、書かないものは触らない）:
//
//   - versioning: Enabled / Suspended。
//   - policy: バケットのポリシー（JSON）。正規形（normalize.go）で比べる。""
//     はポリシーが無いこと（宣言すれば外す）。
//   - lifecycle: ルールの一覧（JSON の配列、各ルールは S3 の XML の Rule を JSON に
//     した形で ID と Status が必須）。正規形で比べる。"" か [] はルールが無いこと。
//   - tags: バケットのタグ（JSON のオブジェクト）。"" か {} はタグが無いこと。
//   - endpoint・bucket: 識別子（ID と同じ値。省略できる）。
//
// 観測だけの値（Computed）: creation_date（ListBuckets の CreationDate）と
// unsupported（endpoint がその API を持たない〈NotImplemented〉フィールドの一覧）。
// unsupported のフィールドは State に入れない。宣言していれば計画の時点で
// 「観測できない」として止め（RefinePlan のエラー、分かる文で）、宣言して
// いなければ無視する。
//
// 削除：Delete は破壊的（prune が Destructive にして予算に数える）。中身がある
// バケットは消さない——オブジェクトを消す機能は持たず、endpoint の
// BucketNotEmpty は PermanentError として知らせる。
//
// 届かない・5xx・認証の失敗は観測できない（Unknown。absent にしない）。List は
// 全 endpoint の ListBuckets が最後まで成功したときだけ ListComplete。
package s3bucket

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

const (
	// Name は provider 名（core.ID.Provider に入る）。
	Name = "s3-bucket"

	// ProviderVersion は diff / 正規化の版（§10.4）。正規形の作り方を変えたら上げる。
	ProviderVersion = Name + "-v1"
)

// フィールド名。
const (
	FieldEndpoint     = "endpoint"
	FieldBucket       = "bucket"
	FieldVersioning   = "versioning"
	FieldPolicy       = "policy"
	FieldLifecycle    = "lifecycle"
	FieldTags         = "tags"
	FieldCreationDate = "creation_date"
	FieldUnsupported  = "unsupported"
)

// versioning の値。
const (
	VersioningEnabled   = "Enabled"
	VersioningSuspended = "Suspended"
)

// ListSep は unsupported（StringList）の区切り。
const ListSep = ","

// apiOf はフィールドを観測する S3 の API の名前（文言用）。
var apiOf = map[string]string{
	FieldVersioning: "GetBucketVersioning",
	FieldPolicy:     "GetBucketPolicy",
	FieldLifecycle:  "GetBucketLifecycleConfiguration",
	FieldTags:       "GetBucketTagging",
}

// Endpoint は 1 つの endpoint（Client と、扱うバケットの接頭辞）。
type Endpoint struct {
	Client *Client
	// BucketPrefix が空でなければ、この接頭辞のバケットだけを扱う（List もこれ
	// だけを返す。prune の閉じた世界をこの接頭辞に限る）。
	BucketPrefix string
}

// Options は Provider の設定。
type Options struct {
	// Scope は List・BulkObserve が返す ID の scope（既定 root、#106 と同じ）。
	Scope core.Scope
}

// Provider は provider.Provider・Lister・BulkObserver・IntentValidator・
// PlanRefiner を実装する。
type Provider struct {
	endpoints map[string]Endpoint
	scope     core.Scope
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.Lister          = (*Provider)(nil)
	_ provider.BulkObserver    = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
)

// New は endpoints（名前 → Endpoint）を使う Provider を返す。
func New(endpoints map[string]Endpoint, opts Options) *Provider {
	eps := make(map[string]Endpoint, len(endpoints))
	for k, v := range endpoints {
		eps[k] = v
	}
	return &Provider{endpoints: eps, scope: opts.Scope}
}

// Schema: identity は endpoint と bucket。観測だけの値は Computed。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldEndpoint:     {Kind: core.FieldKindString, Identity: true},
		FieldBucket:       {Kind: core.FieldKindString, Identity: true},
		FieldVersioning:   {Kind: core.FieldKindString},
		FieldPolicy:       {Kind: core.FieldKindString},
		FieldLifecycle:    {Kind: core.FieldKindString},
		FieldTags:         {Kind: core.FieldKindString},
		FieldCreationDate: {Kind: core.FieldKindString, Computed: true},
		FieldUnsupported:  {Kind: core.FieldKindStringList, Computed: true, ListSep: ListSep},
	}}
}

// Capabilities は完全の階級。Pruneable は true：中身のあるバケットは endpoint が
// 消させない（BucketNotEmpty）ので、prune で失うのは空のバケットだけ。prune
// 自体は PruneWithinScope を宣言したときだけ起き、Delete は破壊的として予算に
// 数える（§12・§13）。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		BulkObserve:     true,
		Pruneable:       true,
		ProviderVersion: ProviderVersion,
	}
}

// ---- ID ----

// bucketName は S3 のバケット名の決まり（3〜63 文字、小文字・数字・"."・"-"、
// 先頭と末尾は英数字）。
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// ValidBucketName は name が S3 のバケット名として使えるかを返す。
func ValidBucketName(name string) bool {
	return bucketName.MatchString(name) && !strings.Contains(name, "..") && net.ParseIP(name) == nil
}

// LocalID は endpoint の名前とバケットから ID を作る。
func LocalID(endpoint, bucket string) string { return endpoint + "/" + bucket }

// coords は ID を endpoint とバケットに分け、扱えるものかを確かめる。
func (p *Provider) coords(localID string) (string, Endpoint, string, error) {
	name, bucket, ok := strings.Cut(localID, "/")
	if !ok || name == "" || bucket == "" {
		return "", Endpoint{}, "", fmt.Errorf("id %q: want <endpoint>/<bucket>", localID)
	}
	ep, ok := p.endpoints[name]
	if !ok {
		return "", Endpoint{}, "", fmt.Errorf("id %q: no endpoint %q is configured (add it to the endpoint list, --s3-config)", localID, name)
	}
	if !ValidBucketName(bucket) {
		return "", Endpoint{}, "", fmt.Errorf("id %q: %q is not a valid bucket name (3-63 characters of a-z 0-9 . -, starting and ending with a letter or digit)", localID, bucket)
	}
	if !strings.HasPrefix(bucket, ep.BucketPrefix) {
		return "", Endpoint{}, "", fmt.Errorf("id %q: endpoint %s only handles buckets starting with %q (bucket_prefix)", localID, name, ep.BucketPrefix)
	}
	return name, ep, bucket, nil
}

func (p *Provider) endpointNames() []string {
	names := make([]string, 0, len(p.endpoints))
	for n := range p.endpoints {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ---- 観測 ----

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}

// Read はバケットを観測する。desired を見ない（ADR-0002）。ListBuckets に無ければ
// absent、届かない・認証の失敗・読む間に消えたのは Unknown。
func (p *Provider) Read(ctx context.Context, localID string) (core.Observation, error) {
	name, ep, bucket, err := p.coords(localID)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read: %w", Name, err))
	}
	sess, err := ep.Client.Open(ctx)
	if err != nil {
		return unknownObs(), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	buckets, err := sess.ListBuckets(ctx)
	if err != nil {
		return unknownObs(), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	for _, b := range buckets {
		if b.Name == bucket {
			st, err := observeBucket(ctx, sess, name, b)
			if err != nil {
				return unknownObs(), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
			}
			return core.Observation{Availability: core.AvailabilityOk, State: st, ObservedAt: time.Now()}, nil
		}
	}
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}

// observeBucket は 1 つのバケットの設定を読んで State にする。endpoint が持た
// ない API のフィールドは State に入れず、unsupported に挙げる。
func observeBucket(ctx context.Context, sess *Session, endpoint string, b BucketInfo) (core.State, error) {
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	st := core.State{
		FieldEndpoint:     str(endpoint),
		FieldBucket:       str(b.Name),
		FieldCreationDate: str(b.CreationDate),
	}
	var unsupported []string
	// skip は NotImplemented を unsupported にし、それ以外のエラーは返す。
	skip := func(field string, err error) (bool, error) {
		if err == nil {
			return false, nil
		}
		if KindOf(err) == KindNotImplemented {
			unsupported = append(unsupported, field)
			return true, nil
		}
		if CodeOf(err) == "NoSuchBucket" {
			// ListBuckets の後に消えた。読む間に現実が変わったので、この周は観測できない。
			return true, &Error{Kind: KindUnreachable, Op: apiOf[field], Bucket: b.Name, Message: "the bucket disappeared while it was being read", Err: err}
		}
		return true, err
	}

	v, err := sess.GetVersioning(ctx, b.Name)
	if s, err := skip(FieldVersioning, err); err != nil {
		return nil, err
	} else if !s {
		st[FieldVersioning] = str(v)
	}

	pol, err := sess.GetPolicy(ctx, b.Name)
	if s, err := skip(FieldPolicy, err); err != nil {
		return nil, err
	} else if !s {
		c, cerr := CanonPolicy(pol)
		if cerr != nil {
			c = strings.TrimSpace(pol) // 読めないものはそのまま（宣言の正規形とは一致しない＝drift として見える）。
		}
		st[FieldPolicy] = str(c)
	}

	lc, err := sess.GetLifecycle(ctx, b.Name)
	if s, err := skip(FieldLifecycle, err); err != nil {
		return nil, err
	} else if !s {
		rules, perr := lifecycleFromXML(lc)
		if perr != nil {
			return nil, &Error{Kind: KindUnreachable, Op: "GetBucketLifecycleConfiguration", Bucket: b.Name, Message: "unreadable response", Err: perr}
		}
		st[FieldLifecycle] = str(canonRules(rules))
	}

	tags, err := sess.GetTagging(ctx, b.Name)
	if s, err := skip(FieldTags, err); err != nil {
		return nil, err
	} else if !s {
		st[FieldTags] = str(CanonTagMap(tags))
	}

	sort.Strings(unsupported)
	st[FieldUnsupported] = core.TypedValue{Kind: core.FieldKindStringList, Value: strings.Join(unsupported, ListSep)}
	return st, nil
}

// List は全 endpoint のバケット（bucket_prefix に合うもの）を返す。全 endpoint の
// ListBuckets が最後まで成功したときだけ ListComplete、1 つでも失敗すれば error
// （complete-or-error、§5.4）。
func (p *Provider) List(ctx context.Context, _ core.Scope) (core.ListResult, error) {
	var ids []core.ID
	for _, name := range p.endpointNames() {
		ep := p.endpoints[name]
		sess, err := ep.Client.Open(ctx)
		if err != nil {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", Name, name, err))
		}
		buckets, err := sess.ListBuckets(ctx)
		if err != nil {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", Name, name, err))
		}
		for _, b := range buckets {
			if strings.HasPrefix(b.Name, ep.BucketPrefix) {
				ids = append(ids, core.NewID(Name, p.scope, LocalID(name, b.Name)))
			}
		}
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve は endpoint ごとに ListBuckets を 1 回だけ呼び、各バケットの設定を
// 読む（§5.7）。各エントリは Read が返すものと同じ。1 つのバケットの設定が
// 読めなければ、そのエントリは Unknown。
//
// ListBuckets が失敗した endpoint は結果に含めない（その endpoint のバケットは
// 「この呼び出しでは観測していない」で、SDK が Read に回し、Read が Unknown に
// する。absent にはならない）。1 つの endpoint が止まっても、ほかの endpoint の
// バケットは観測を続ける。全 endpoint が失敗したときだけ error を返す（全部を
// 1 つずつ Read させない）。prune の完全性は List が別に守る（1 つでも失敗すれば
// error）。
func (p *Provider) BulkObserve(ctx context.Context, _ core.Scope) (provider.BulkObservation, error) {
	obs := map[core.IDKey]core.Observation{}
	var firstErr error
	failed := 0
	for _, name := range p.endpointNames() {
		ep := p.endpoints[name]
		sess, err := ep.Client.Open(ctx)
		var buckets []BucketInfo
		if err == nil {
			buckets, err = sess.ListBuckets(ctx)
		}
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = classify(fmt.Errorf("%s: BulkObserve %s: %w", Name, name, err))
			}
			continue
		}
		for _, b := range buckets {
			if !strings.HasPrefix(b.Name, ep.BucketPrefix) {
				continue
			}
			key := core.NewID(Name, p.scope, LocalID(name, b.Name)).Key()
			st, err := observeBucket(ctx, sess, name, b)
			if err != nil {
				obs[key] = unknownObs()
				continue
			}
			obs[key] = core.Observation{Availability: core.AvailabilityOk, State: st, ObservedAt: time.Now()}
		}
	}
	if failed > 0 && failed == len(p.endpoints) {
		return provider.BulkObservation{}, firstErr
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// ---- 計画の時点の検査 ----

// ValidateIntent は現実を見ずに決まる誤りを、読み込みの時点で断る（§28.4）。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	name, _, bucket, err := p.coords(localID)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	schema := p.Schema()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec, ok := schema.Fields[k]
		switch {
		case !ok:
			return fmt.Errorf("%s %q: unknown field %q", Name, localID, k)
		case spec.Computed:
			return fmt.Errorf("%s %q: field %q is observed only and cannot be declared", Name, localID, k)
		}
	}
	if v, ok := fields[FieldEndpoint]; ok && v != name {
		return fmt.Errorf("%s %q: endpoint %q must equal the endpoint in the id (%s)", Name, localID, v, name)
	}
	if v, ok := fields[FieldBucket]; ok && v != bucket {
		return fmt.Errorf("%s %q: bucket %q must equal the bucket in the id (%s)", Name, localID, v, bucket)
	}
	if v, ok := fields[FieldVersioning]; ok && v != VersioningEnabled && v != VersioningSuspended {
		return fmt.Errorf("%s %q: versioning %q: want %s or %s", Name, localID, v, VersioningEnabled, VersioningSuspended)
	}
	if v, ok := fields[FieldPolicy]; ok {
		if _, err := CanonPolicy(v); err != nil {
			return fmt.Errorf("%s %q: %w", Name, localID, err)
		}
	}
	if v, ok := fields[FieldLifecycle]; ok {
		if _, err := ParseLifecycle(v); err != nil {
			return fmt.Errorf("%s %q: %w", Name, localID, err)
		}
	}
	if v, ok := fields[FieldTags]; ok {
		tags, err := ParseTags(v)
		if err != nil {
			return fmt.Errorf("%s %q: %w", Name, localID, err)
		}
		if len(tags) > 50 {
			return fmt.Errorf("%s %q: tags: %d tags (S3 allows at most 50)", Name, localID, len(tags))
		}
		for k, tv := range tags {
			if len(k) > 128 || len(tv) > 256 {
				return fmt.Errorf("%s %q: tags: the key %q or its value is too long (key at most 128, value at most 256)", Name, localID, k)
			}
		}
	}
	return nil
}

// canonField は宣言の値の正規形（versioning などはそのまま）。
func canonField(field, v string) (string, error) {
	switch field {
	case FieldPolicy:
		return CanonPolicy(v)
	case FieldLifecycle:
		return CanonLifecycle(v)
	case FieldTags:
		return CanonTags(v)
	}
	return v, nil
}

// RefinePlan は core の差分を S3 の決まりで直す（§28.4）。I/O はしない。
//
//   - 宣言したフィールドの API を endpoint が持たない（観測の unsupported に
//     ある）なら、観測できないとしてエラー（このリソースはこの周、何もしない）。
//   - policy・lifecycle・tags は正規形が観測と同じなら差分から落とす（キー順・
//     空白・書き方の違いを drift にしない）。
func (p *Provider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	var missing []string
	for _, f := range observedList(observed, FieldUnsupported) {
		if _, declared := desired[f]; declared {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		var apis []string
		for _, f := range missing {
			apis = append(apis, apiOf[f])
		}
		return nil, fmt.Errorf("%s %q: cannot observe %s: the endpoint does not implement %s (NotImplemented); remove the field from the intent or use an endpoint that has the API",
			Name, localID, strings.Join(missing, ", "), strings.Join(apis, ", "))
	}
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		if op.Kind == core.OpKindUpdate {
			var fields []core.FieldDiff
			for _, f := range op.Fields {
				if c, err := canonField(f.Field, f.To); err == nil && c == f.From {
					continue
				}
				fields = append(fields, f)
			}
			op.Fields = fields
		}
		out = append(out, op)
	}
	return out, nil
}

func observedList(s core.State, field string) []string {
	tv, ok := s[field]
	if !ok {
		return nil
	}
	v := fmt.Sprintf("%v", tv.Value)
	if v == "" {
		return nil
	}
	return strings.Split(v, ListSep)
}

// ---- Apply ----

// Apply は plan を適用する。Create はバケットを作ってから宣言したフィールドを
// 設定し、Update は差分のフィールドだけを設定し、Delete は空のバケットだけを
// 消す。どれも同じ plan を 2 回適用しても結果は同じ。policy・lifecycle・tags は
// 設定した後に読み戻し、endpoint が宣言と違う形で持つなら PermanentError（その
// 形を添える。永久 drift で Apply を繰り返さない）。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) applyOp(ctx context.Context, op core.Op) error {
	localID := op.Target.Local
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: "+format, append([]any{Name, localID}, args...)...)
	}
	_, ep, bucket, err := p.coords(localID)
	if err != nil {
		return provider.NewPermanentError(fail("%w", err))
	}
	sess, err := ep.Client.Open(ctx)
	if err != nil {
		return classify(fail("%w", err))
	}
	switch op.Kind {
	case core.OpKindDelete:
		err := sess.DeleteBucket(ctx, bucket)
		if CodeOf(err) == "BucketNotEmpty" {
			return provider.NewPermanentError(fail("the bucket is not empty, so it is not deleted (Sakutei never deletes objects; empty it by hand to let it go, or declare it again to keep it): %w", err))
		}
		if err != nil {
			return classify(fail("%w", err))
		}
		return nil
	case core.OpKindCreate:
		if err := sess.CreateBucket(ctx, bucket); err != nil {
			if CodeOf(err) == "BucketAlreadyExists" {
				return provider.NewPermanentError(fail("the bucket name is taken by another account: %w", err))
			}
			return classify(fail("%w", err))
		}
	}
	for _, f := range op.Fields {
		if err := p.applyField(ctx, sess, bucket, f.Field, f.To); err != nil {
			return fail("%s: %w", f.Field, err)
		}
	}
	return nil
}

func (p *Provider) applyField(ctx context.Context, sess *Session, bucket, field, to string) error {
	switch field {
	case FieldEndpoint, FieldBucket:
		return nil // 識別子（ID が決める）。
	case FieldVersioning:
		if to != VersioningEnabled && to != VersioningSuspended {
			return provider.NewPermanentError(fmt.Errorf("%q cannot be applied (want %s or %s)", to, VersioningEnabled, VersioningSuspended))
		}
		return putErr(sess.PutVersioning(ctx, bucket, to))
	case FieldPolicy:
		want, err := CanonPolicy(to)
		if err != nil {
			return provider.NewPermanentError(err)
		}
		if want == "" {
			err = sess.DeletePolicy(ctx, bucket)
		} else {
			err = sess.PutPolicy(ctx, bucket, to)
		}
		if err != nil {
			return putErr(err)
		}
		got, err := sess.GetPolicy(ctx, bucket)
		if err != nil {
			return classify(err)
		}
		return readBack(field, want, got, CanonPolicy)
	case FieldLifecycle:
		rules, err := ParseLifecycle(to)
		if err != nil {
			return provider.NewPermanentError(err)
		}
		if len(rules) == 0 {
			err = sess.DeleteLifecycle(ctx, bucket)
		} else {
			err = sess.PutLifecycle(ctx, bucket, lifecycleXML(rules))
		}
		if err != nil {
			return putErr(err)
		}
		data, err := sess.GetLifecycle(ctx, bucket)
		if err != nil {
			return classify(err)
		}
		gotRules, err := lifecycleFromXML(data)
		if err != nil {
			return provider.NewTransientError(err)
		}
		return readBack(field, canonRules(rules), canonRules(gotRules), nil)
	case FieldTags:
		tags, err := ParseTags(to)
		if err != nil {
			return provider.NewPermanentError(err)
		}
		if len(tags) == 0 {
			err = sess.DeleteTagging(ctx, bucket)
		} else {
			err = sess.PutTagging(ctx, bucket, tags)
		}
		if err != nil {
			return putErr(err)
		}
		got, err := sess.GetTagging(ctx, bucket)
		if err != nil {
			return classify(err)
		}
		return readBack(field, CanonTagMap(tags), CanonTagMap(got), nil)
	}
	return provider.NewPermanentError(fmt.Errorf("unknown field %q", field))
}

// readBack は設定した後に読み戻した値（正規形）を宣言の正規形と比べる。
func readBack(field, want, got string, canon func(string) (string, error)) error {
	if canon != nil {
		c, err := canon(got)
		if err != nil {
			c = strings.TrimSpace(got)
		}
		got = c
	}
	if got == want {
		return nil
	}
	return provider.NewPermanentError(fmt.Errorf("the endpoint accepted the %s but keeps it in another form, so it would never compare equal; write it in the form the endpoint keeps: %s", field, got))
}

// putErr は設定の要求のエラーを分ける。endpoint がその API を持たなければ
// PermanentError（分かる文で）。
func putErr(err error) error {
	if err == nil {
		return nil
	}
	if KindOf(err) == KindNotImplemented {
		return provider.NewPermanentError(fmt.Errorf("the endpoint does not implement this API (NotImplemented), so the field cannot be managed there: %w", err))
	}
	return classify(err)
}

// transientCodes は時間が経てば治る S3 のエラーの Code。
var transientCodes = map[string]bool{
	"OperationAborted": true, "SlowDown": true, "RequestTimeout": true, "InternalError": true, "ServiceUnavailable": true,
}

// classify は Client のエラーを provider のエラー型にする。届かない・5xx・時間
// 切れ・一時的な Code は TransientError、認証・証明書・資格・拒まれた要求は
// 設定を直すまで治らないので PermanentError。
func classify(err error) error {
	if err == nil {
		return nil
	}
	if KindOf(err) == KindUnreachable || transientCodes[CodeOf(err)] {
		return provider.NewTransientError(err)
	}
	return provider.NewPermanentError(err)
}
