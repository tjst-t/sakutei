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

// Package goldenmin は最小階級（§5.3）の provider テンプレート。Schema → Read →
// Apply だけを持つ。対象の現在値を返せない（TrueObserve:false）ため、Read は
// 存在だけを返し、コアの diff・原因タグは機械的に縮退する。List が無いので
// prune はできない（open-world）。
//
// 使い方は goldenfull と同じ：コピーして "goldenmin" を自分の provider 名に
// 置き換え、Client を実装する（templates/provider/README.md）。
package goldenmin

import (
	"context"
	"fmt"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// Name は provider 名。
	Name = "goldenmin"
	// ProviderVersion は diff / 正規化の版（§10.4）。
	ProviderVersion = Name + "-v1"
)

// Client は対象との境界。エラーは「到達できなかった」ことを表す（unknown ≠ absent）。
type Client interface {
	Exists(ctx context.Context, name string) (bool, error)
	// Ensure は fields の状態にする（冪等）。changed は変えたかどうか。
	Ensure(ctx context.Context, name string, fields map[string]string) (changed bool, err error)
	// Remove は存在しなくても成功する（冪等）。
	Remove(ctx context.Context, name string) (changed bool, err error)
}

// Provider は provider.Provider を実装する。
type Provider struct{ client Client }

var _ provider.Provider = (*Provider)(nil)

// New は client を使う Provider を返す。
func New(client Client) *Provider { return &Provider{client: client} }

// Schema はフィールドを宣言する（§28.4）。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		"name":  {Kind: core.FieldKindString, Required: true, Identity: true},
		"value": {Kind: core.FieldKindString, Required: true},
	}}
}

// Capabilities は最小階級：観測も列挙もできない。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: ProviderVersion}
}

// Read は存在だけを観測する（identity のみの State か、absent なら nil）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	ok, err := p.client.Exists(ctx, id)
	if err != nil {
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()},
			provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	obs := core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}
	if ok {
		obs.State = core.State{"name": {Kind: core.FieldKindString, Value: id}}
	}
	return obs, nil
}

// Apply は plan を冪等に適用する。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		name := op.Target.Local
		var err error
		if op.Kind == core.OpKindDelete {
			_, err = p.client.Remove(ctx, name)
		} else {
			fields := make(map[string]string, len(op.Fields))
			for _, f := range op.Fields {
				fields[f.Field] = f.To
			}
			_, err = p.client.Ensure(ctx, name, fields)
		}
		if err != nil {
			return provider.NewTransientError(fmt.Errorf("%s: Apply %q: %w", Name, name, err))
		}
	}
	return nil
}
