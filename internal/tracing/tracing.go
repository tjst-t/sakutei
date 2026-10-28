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

// Package tracing はトレースの出口の span（Backlog #189、§26.8）。周・観測・
// plan・Apply・Workflow の段を span にする。送るのは internal/otlpexport
// （OTLP/HTTP の JSON の形）。どこからでも呼べるよう、このパッケージは標準の
// ライブラリだけに依る。外の依存は入れない（OpenTelemetry の SDK も protobuf も
// 使わない）。
//
// 決まり:
//
//   - 既定は出さない。--otlp-endpoint を明示したときだけ Tracer を作る。
//     OTEL_EXPORTER_OTLP_* などの環境変数は読まない（黙って外へ出さない）。
//   - Tracer の無い context では Start は何もしない（nil の *Span を返し、その
//     メソッドは何もしない）。呼ぶ側は出す・出さないを気にしなくてよい。
//   - span には名前・ID・数・結果だけを載せる。欄の値（Diff の中身・秘密）は
//     載せない。
//   - 送れなくても収束は止めない・待たない：span は上限つきの列に積み、別の
//     goroutine が束にして送る。列が溢れたら捨てて数える。
//   - 送る形と宛先の TLS・認可は internal/otlpexport（読み取りの口 #161 と同じ考え）。
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// span の名前（§26.8）。名前は種類だけ（濃度を低く）で、どれかは属性で言う。
const (
	SpanCycle        = "sakutei.cycle"         // 常駐の 1 周
	SpanReconcile    = "sakutei.reconcile"     // 一回性の reconcile
	SpanPlan         = "sakutei.plan"          // 一回性の plan と、周の中の差分・予算の段
	SpanObserve      = "sakutei.observe"       // 周の中の観測の段
	SpanRead         = "sakutei.read"          // 1 つのリソースの観測（Read）
	SpanBulkObserve  = "sakutei.bulk_observe"  // provider の BulkObserve の 1 回
	SpanApply        = "sakutei.apply"         // provider の Apply の 1 回（prune の Delete を含む）
	SpanWorkflow     = "sakutei.workflow"      // 1 つの Workflow のその周の進み
	SpanWorkflowStep = "sakutei.workflow.step" // Workflow の手順の 1 つ
)

// 属性の名前。
const (
	AttrCycle     = "sakutei.cycle.number"
	AttrProvider  = "sakutei.provider"
	AttrScope     = "sakutei.scope"
	AttrResource  = "sakutei.resource"
	AttrResources = "sakutei.resources"
	AttrOps       = "sakutei.ops"
	AttrOpKinds   = "sakutei.op_kinds"
	AttrCauses    = "sakutei.causes"
	AttrPrune     = "sakutei.prune"
	AttrWorkflow  = "sakutei.workflow"
	AttrRun       = "sakutei.workflow.run"
	AttrStage     = "sakutei.workflow.stage"
	AttrTarget    = "sakutei.workflow.target"
	AttrStep      = "sakutei.workflow.step"
	AttrStepKind  = "sakutei.workflow.step_kind"
	AttrOutcome   = "sakutei.outcome"
)

// Attr は span の属性。Value は string・int・int64・bool・float64・[]string。
type Attr struct {
	Key   string
	Value any
}

// String・Int・Bool・Strings は属性を作る。
func String(k, v string) Attr    { return Attr{k, v} }
func Int(k string, v int) Attr   { return Attr{k, int64(v)} }
func Bool(k string, v bool) Attr { return Attr{k, v} }
func Strings(k string, v []string) Attr {
	return Attr{k, append([]string(nil), v...)}
}

// Sink は閉じた span の受け手（OTLP の exporter、internal/otlpexport）。
// Enqueue は待たずに返らなければならない（送れなくても収束を止めない）。
type Sink interface {
	Enqueue(SpanData)
}

// Tracer は span を作って Sink に渡す。並行に使ってよい。
type Tracer struct {
	sink Sink
	now  func() time.Time
}

// NewTracer は sink へ渡す Tracer。
func NewTracer(sink Sink) *Tracer {
	return &Tracer{sink: sink, now: time.Now}
}

type ctxKey int

const (
	tracerKey ctxKey = iota
	spanKey
)

// WithTracer は t で span を作る context。t が nil なら ctx のまま（出さない）。
func WithTracer(ctx context.Context, t *Tracer) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, tracerKey, t)
}

// Enabled は ctx で span が作られるなら true。
func Enabled(ctx context.Context) bool {
	_, ok := ctx.Value(tracerKey).(*Tracer)
	return ok
}

// Span は 1 つの span。nil の *Span のメソッドは何もしない。
type Span struct {
	t        *Tracer
	traceID  [16]byte
	spanID   [8]byte
	parentID [8]byte
	name     string
	start    time.Time

	mu     sync.Mutex
	attrs  []Attr
	errMsg string
	failed bool
	ended  bool
}

// Start は ctx の span の子（無ければ新しいトレースの根）の span を始め、それを
// 載せた context を返す。ctx に Tracer が無ければ ctx と nil を返す。
func Start(ctx context.Context, name string, attrs ...Attr) (context.Context, *Span) {
	t, ok := ctx.Value(tracerKey).(*Tracer)
	if !ok {
		return ctx, nil
	}
	s := &Span{t: t, name: name, start: t.now(), attrs: append([]Attr(nil), attrs...)}
	if parent, ok := ctx.Value(spanKey).(*Span); ok {
		s.traceID, s.parentID = parent.traceID, parent.spanID
	} else {
		_, _ = rand.Read(s.traceID[:])
	}
	_, _ = rand.Read(s.spanID[:])
	return context.WithValue(ctx, spanKey, s), s
}

// SetAttributes は属性を足す（同じ名前は後のものが勝つ）。
func (s *Span) SetAttributes(attrs ...Attr) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = append(s.attrs, attrs...)
}

// Fail は span を失敗にする（status ERROR）。err が nil なら何もしない。
// 文言は provider の誤りの文で、秘密の値は含まない（§20 の決まり）。
func (s *Span) Fail(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed, s.errMsg = true, err.Error()
}

// End は span を閉じて送る列に積む。2 度目からは何もしない。
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	d := SpanData{
		TraceID: hex.EncodeToString(s.traceID[:]), SpanID: hex.EncodeToString(s.spanID[:]),
		Name: s.name, Start: s.start, End: s.t.now(), Attrs: s.attrs, Failed: s.failed, ErrMsg: s.errMsg,
	}
	if s.parentID != ([8]byte{}) {
		d.ParentID = hex.EncodeToString(s.parentID[:])
	}
	s.mu.Unlock()
	s.t.sink.Enqueue(d)
}

// SpanData は閉じた span（送る形の手前）。ID は 16 進。
type SpanData struct {
	TraceID, SpanID, ParentID string
	Name                      string
	Start, End                time.Time
	Attrs                     []Attr
	Failed                    bool
	ErrMsg                    string
}
