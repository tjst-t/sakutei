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

package system

// media.go は Redfish の BMC provider のリソース「仮想メディア」（CD）。設計書
// v3.16 §29.1「Redfish の BMC provider」の（2）（6）、Backlog #48。システムと同じ
// パッケージに別の型（MediaProvider）として置き、ノードごとの redfish.Client
// を共有する。標準階級で List と BulkObserve を持たない（CD のドライブは BMC に
// 前からあり、Sakutei が作ったり消したりしないので prune の対象にしない）。
//
// ID は "<ノード名>/cd"（例 "node1/cd"）。BMC はノード名の部分で引く。ノード名
// そのものにしないのは、Local ID が intent 全体で一意（depends_on の名前でもある）
// で、同じノードの「システム」（ID はノード名）と並べて宣言するため。
//
// 宣言するフィールド:
//
//   - name: ノード名（ID のノード名の部分と同じ）。
//   - image: URL（http(s):// など）か BMC から見える絶対パス。空文字なら取り
//     出した状態。
//
// 観測: Inserted が false なら image は空として比べる（BMC が取得に失敗して
// Inserted を false に戻したとき、次の周にずれとして入れ直す。続けばずれの
// level が stuck に上がる、§22.3）。BMC が返す生の Image と Inserted は観測
// だけの値（media_image・inserted、Computed）。
//
// Apply は BMC を読み直して今の現実とずれているときだけ操作する: 違う image が
// 入っていれば同じ Apply の中で Eject → Insert、空を宣言して入っていれば
// Eject、入っていなければ Insert。仮想メディアの操作は破壊的に数えない。

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// MediaName は仮想メディアの provider 名（core.ID.Provider に入る）。
	MediaName = "redfish-virtual-media"

	// MediaProviderVersion は diff / 正規化の版（§10.4）。
	MediaProviderVersion = MediaName + "-v1"
)

// MediaIDSuffix は仮想メディアの ID の末尾（"<ノード名>/cd"）。
const MediaIDSuffix = "/cd"

// MediaID はノード node の仮想メディアの ID を返す。
func MediaID(node string) string { return node + MediaIDSuffix }

// mediaNode は仮想メディアの ID からノード名を取り出す。
func mediaNode(id string) (string, error) {
	node := strings.TrimSuffix(id, MediaIDSuffix)
	if node == id || node == "" {
		return "", fmt.Errorf("%s: id %q: want \"<node>%s\" (e.g. %q)", MediaName, id, MediaIDSuffix, MediaID("node1"))
	}
	return node, nil
}

// 仮想メディアのフィールド名（name は FieldName と同じ）。
const (
	FieldImage      = "image"
	FieldInserted   = "inserted"
	FieldMediaImage = "media_image"
)

// MediaProvider は provider.Provider と IntentValidator を実装する。
type MediaProvider struct {
	clients map[string]redfish.Client
}

var (
	_ provider.Provider        = (*MediaProvider)(nil)
	_ provider.IntentValidator = (*MediaProvider)(nil)
)

// NewMedia は clients（ノード名 → そのノードの BMC の Client）を使う
// MediaProvider を返す。
func NewMedia(clients map[string]redfish.Client) *MediaProvider {
	cs := make(map[string]redfish.Client, len(clients))
	for k, v := range clients {
		cs[k] = v
	}
	return &MediaProvider{clients: cs}
}

// Schema は §29.1 の（6）。identity はノード名だけ。
func (p *MediaProvider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldName:       {Kind: core.FieldKindString, Required: true, Identity: true},
		FieldImage:      {Kind: core.FieldKindString},
		FieldInserted:   {Kind: core.FieldKindBool, Computed: true},
		FieldMediaImage: {Kind: core.FieldKindString, Computed: true},
	}}
}

// Capabilities は標準階級: List と BulkObserve は無く、消さない。
func (p *MediaProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            false,
		BulkObserve:     false,
		Pruneable:       false,
		ProviderVersion: MediaProviderVersion,
	}
}

// Read はノード id の仮想メディア（CD）を観測する。desired を見ない（ADR-0002）。
func (p *MediaProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	now := time.Now()
	node, err := mediaNode(id)
	if err != nil {
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now}, provider.NewPermanentError(err)
	}
	c, ok := p.clients[node]
	if !ok {
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now},
			provider.NewPermanentError(fmt.Errorf("%s: Read %q: no BMC is configured for this node", MediaName, id))
	}
	vm, err := c.GetVirtualMedia(ctx)
	if err != nil {
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now},
			classify(fmt.Errorf("%s: Read %q: %w", MediaName, id, err))
	}
	return core.Observation{Availability: core.AvailabilityOk, State: mediaState(node, vm), ObservedAt: now}, nil
}

// mediaState は観測を State にする。Inserted が false なら image は空。
func mediaState(node string, vm redfish.VirtualMedia) core.State {
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	return core.State{
		FieldName:       str(node),
		FieldImage:      str(insertedImage(vm)),
		FieldInserted:   {Kind: core.FieldKindBool, Value: vm.Inserted},
		FieldMediaImage: str(vm.Image),
	}
}

func insertedImage(vm redfish.VirtualMedia) string {
	if !vm.Inserted {
		return ""
	}
	return vm.Image
}

// imageForm は image に書けるもの: URL（scheme://…）か絶対パス。
var imageForm = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://.+|/.*)$`)

// ValidateIntent は現実を見ずに決まる誤りを、ロードの時点で弾く。
func (p *MediaProvider) ValidateIntent(localID string, fields map[string]string) error {
	node, err := mediaNode(localID)
	if err != nil {
		return err
	}
	schema := p.Schema()
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		spec, ok := schema.Fields[k]
		switch {
		case !ok:
			return fmt.Errorf("%s %q: unknown field %q", MediaName, localID, k)
		case spec.Computed:
			return fmt.Errorf("%s %q: field %q is observed only and cannot be declared", MediaName, localID, k)
		}
	}
	if v, ok := fields[FieldName]; ok && v != node {
		return fmt.Errorf("%s %q: name %q must equal the node name in the resource id (%q)", MediaName, localID, v, node)
	}
	if v := fields[FieldImage]; v != "" && !imageForm.MatchString(v) {
		return fmt.Errorf("%s %q: image %q: want a URL (such as https://host/x.iso) or an absolute path the BMC can see; an empty image means ejected", MediaName, localID, v)
	}
	if _, ok := p.clients[node]; !ok {
		return fmt.Errorf("%s %q: no BMC is configured for node %q (add it to the BMC list, --redfish-config)", MediaName, localID, node)
	}
	return nil
}

// Apply は plan を適用する。BMC を読み直し、今の現実とずれているときだけ
// 操作する（同じ plan を 2 回適用しても結果は同じ）。
func (p *MediaProvider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *MediaProvider) applyOp(ctx context.Context, op core.Op) error {
	id := op.Target.Local
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: "+format, append([]any{MediaName, id}, args...)...)
	}
	node, err := mediaNode(id)
	if err != nil {
		return provider.NewPermanentError(err)
	}
	if op.Kind == core.OpKindDelete {
		return provider.NewPermanentError(fail("virtual media is never deleted (declare image \"\" to eject)"))
	}
	c, ok := p.clients[node]
	if !ok {
		return provider.NewPermanentError(fail("no BMC is configured for this node"))
	}
	want, ok := "", false
	for _, f := range op.Fields {
		if f.Field == FieldImage {
			want, ok = f.To, true
		}
	}
	if !ok {
		return nil
	}
	vm, err := c.GetVirtualMedia(ctx)
	if err != nil {
		return classify(fail("%w", err))
	}
	have := insertedImage(vm)
	if have == want {
		return nil
	}
	if vm.Inserted {
		if err := c.EjectMedia(ctx); err != nil {
			return classify(fail("EjectMedia: %w", err))
		}
	}
	if want == "" {
		return nil
	}
	if err := c.InsertMedia(ctx, want); err != nil {
		return classify(fail("InsertMedia %q: %w", want, err))
	}
	return nil
}
