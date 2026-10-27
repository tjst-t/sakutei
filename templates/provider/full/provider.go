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

// Package goldenfull は provider の golden path テンプレート（完全階級 + bulk、
// §5.3 / §28.6）。Schema → Read → Apply → List → BulkObserve の骨格だけを持つ。
//
// 使い方（templates/provider/README.md も参照）:
//
//  1. このディレクトリを丸ごとコピーし、"goldenfull" を自分の provider 名に
//     置き換える（パッケージ名・Name・ProviderVersion がこの1語から作られている）。
//  2. Client を実機の API クライアントで実装する（Provider 側は触らずに済む）。
//  3. Schema と toState / recordFromFields を対象のフィールドに合わせて書き換える。
//  4. go test — 付属の契約テスト（provider_test.go）がそのまま回る。
//
// 置き換え直後の状態でもビルドでき、契約テストが合格する（tests/acceptance/provider_template_test.go [#33-1]）。
package goldenfull

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// Name は provider 名（core.ID.Provider に入る）。
	Name = "goldenfull"

	// ProviderVersion は diff / 正規化の版（§10.4）。toState や比較の仕方を
	// 変えたら上げる——上げ忘れると ghost diff（cause C）がドリフト（B）に化ける。
	ProviderVersion = Name + "-v1"
)

// Record は対象側から見た 1 リソース。実機の API の形に合わせて書き換える。
type Record struct {
	Name  string
	Value string
	Size  int64
}

// Client は対象（実機の API）との境界。Provider はこれ越しにしか対象に触らない。
// テストは fake_test.go のインメモリ実装を使う。
//
// エラーは「到達できなかった」ことを表す。存在しないことはエラーではなく
// found=false で返す（unknown ≠ absent、§4.5）。
type Client interface {
	Get(ctx context.Context, name string) (rec Record, found bool, err error)
	Put(ctx context.Context, rec Record) error
	// Delete は存在しなくても成功する（冪等）。
	Delete(ctx context.Context, name string) error
	// ListAll は対象の全件を返す。ページングは尽きるまで回し、途中で失敗したら
	// 部分結果ではなくエラーを返す（complete-or-error、§5.4）。
	ListAll(ctx context.Context) ([]Record, error)
}

// Provider は provider.Provider / Lister / BulkObserver を実装する。
type Provider struct {
	client Client
}

var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.Lister       = (*Provider)(nil)
	_ provider.BulkObserver = (*Provider)(nil)
)

// New は client を使う Provider を返す。
func New(client Client) *Provider { return &Provider{client: client} }

// Schema は最難関（§28.4）。identity は運用上の変更頻度で選び、secret / managed /
// computed を正直に宣言する。sdk.LintSchema が明らかな誤りを起動時に弾く。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		"name":  {Kind: core.FieldKindString, Required: true, Identity: true},
		"value": {Kind: core.FieldKindString, Required: true},
		"size":  {Kind: core.FieldKindInt},
	}}
}

// Capabilities は実装を正直に反映する。Pruneable は型単位の判断で、破壊的な型
// （データを失う・停止する）は false のままにする（§12.3）。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		BulkObserve:     true,
		Pruneable:       false,
		ProviderVersion: ProviderVersion,
	}
}

// Read は id のリソースを観測する。desired を見てはいけない（ADR-0002）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	rec, found, err := p.client.Get(ctx, id)
	if err != nil {
		// 到達できない → Unknown。absent（Ok + nil State）と混同しない。
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()},
			provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	if !found {
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
	}
	return core.Observation{Availability: core.AvailabilityOk, State: toState(rec), ObservedAt: time.Now()}, nil
}

// Apply は plan を適用する。同じ plan を 2 回適用しても結果が同じであること（冪等）。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		name := op.Target.Local
		switch op.Kind {
		case core.OpKindCreate, core.OpKindUpdate:
			// Update の Fields は変わったフィールドだけなので、今の値に重ねる。
			cur, _, err := p.client.Get(ctx, name)
			if err != nil {
				return provider.NewTransientError(fmt.Errorf("%s: Apply %q: %w", Name, name, err))
			}
			cur.Name = name
			rec, err := recordFromFields(cur, op.Fields)
			if err != nil {
				return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %w", Name, name, err))
			}
			if err := p.client.Put(ctx, rec); err != nil {
				return provider.NewTransientError(fmt.Errorf("%s: Apply %q: %w", Name, name, err))
			}
		case core.OpKindDelete:
			if err := p.client.Delete(ctx, name); err != nil {
				return provider.NewTransientError(fmt.Errorf("%s: Apply delete %q: %w", Name, name, err))
			}
		default:
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: unknown op kind %v", Name, name, op.Kind))
		}
	}
	return nil
}

// List は scope の全 ID を返す（prune の集合差の入力、§5.4）。全件を取れたときだけ
// ListComplete を名乗る。
func (p *Provider) List(ctx context.Context, scope core.Scope) (core.ListResult, error) {
	recs, err := p.client.ListAll(ctx)
	if err != nil {
		return core.ListResult{Status: core.ListUnknown},
			provider.NewTransientError(fmt.Errorf("%s: List: %w", Name, err))
	}
	ids := make([]core.ID, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, core.NewID(Name, scope, r.Name))
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve は scope を 1 回で観測する（§5.7）。Read と同じ toState を通すこと——
// 経路で現実が変わらないことを付属の等価性テストが確かめる。
func (p *Provider) BulkObserve(ctx context.Context, scope core.Scope) (provider.BulkObservation, error) {
	recs, err := p.client.ListAll(ctx)
	if err != nil {
		return provider.BulkObservation{}, provider.NewTransientError(fmt.Errorf("%s: BulkObserve: %w", Name, err))
	}
	now := time.Now()
	obs := make(map[core.IDKey]core.Observation, len(recs))
	for _, r := range recs {
		obs[core.NewID(Name, scope, r.Name).Key()] = core.Observation{
			Availability: core.AvailabilityOk, State: toState(r), ObservedAt: now,
		}
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// toState は Record を Schema どおりの State にする（Read と BulkObserve で共有）。
func toState(r Record) core.State {
	return core.State{
		"name":  {Kind: core.FieldKindString, Value: r.Name},
		"value": {Kind: core.FieldKindString, Value: r.Value},
		"size":  {Kind: core.FieldKindInt, Value: r.Size},
	}
}

// recordFromFields は plan のフィールドを rec に重ねる。
func recordFromFields(rec Record, fields []core.FieldDiff) (Record, error) {
	for _, f := range fields {
		switch f.Field {
		case "name":
			// identity は ID から決まる。
		case "value":
			rec.Value = f.To
		case "size":
			if f.To == "" {
				rec.Size = 0
				continue
			}
			n, err := strconv.ParseInt(f.To, 10, 64)
			if err != nil {
				return rec, fmt.Errorf("size %q: %w", f.To, err)
			}
			rec.Size = n
		default:
			return rec, fmt.Errorf("unknown field %q", f.Field)
		}
	}
	return rec, nil
}
