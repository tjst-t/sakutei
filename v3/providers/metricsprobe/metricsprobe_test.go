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

package metricsprobe_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/metricsprobe"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const token = "tok-5d1e9c"

const page = `# HELP rabbitmq_queue_messages Messages in the queue.
# TYPE rabbitmq_queue_messages gauge
rabbitmq_queue_messages{queue="orders",vhost="/"} 42
rabbitmq_queue_messages{queue="billing",vhost="/"} 8
up 1
`

type exporter struct {
	srv *httptest.Server
	mu  sync.Mutex
	// fault は応答の差し替え（"" なら page を返す）。
	fault string
	body  string
}

func (e *exporter) set(fault string) { e.mu.Lock(); e.fault = fault; e.mu.Unlock() }

func newExporter(t *testing.T) (*exporter, *metricsprobe.Provider) {
	t.Helper()
	e := &exporter{body: page}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		fault, body := e.fault, e.body
		e.mu.Unlock()
		switch fault {
		case "down":
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
		case "5xx":
			w.WriteHeader(http.StatusBadGateway)
			return
		case "slow":
			select {
			case <-time.After(time.Second):
			case <-r.Context().Done():
			}
			return
		case "redirect":
			http.Redirect(w, r, "http://other.example/metrics", http.StatusFound)
			return
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
			return
		case "broken":
			io.WriteString(w, "rabbitmq_queue_messages{queue=\"orders\" 42\n")
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(e.srv.Close)
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	os.WriteFile(tokFile, []byte(token+"\n"), 0o600)
	p, err := metricsprobe.Module(map[string]any{"endpoints": map[string]any{
		"mq": map[string]any{"url": e.srv.URL + "/metrics", "bearer_token_file": tokFile, "timeout": "300ms"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return e, p.(*metricsprobe.Provider)
}

func series(name string) core.ID { return core.ID{Type: "metrics/series", Name: name} }

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		e, p := newExporter(t)
		return contract.Target{
			Provider:  p,
			Resources: []core.Resource{{ID: series(`mq/rabbitmq_queue_messages{queue="orders"}`)}},
			Observed:  []core.ID{series(`mq/rabbitmq_queue_messages{queue="orders"}`), series("mq/rabbitmq_queue_messages"), series("mq/up")},
			Faults: map[string]func(){
				"接続を切る":  func() { e.set("down") },
				"5xx":    func() { e.set("5xx") },
				"時間切れ":   func() { e.set("slow") },
				"リダイレクト": func() { e.set("redirect") },
				"壊れた出力":  func() { e.set("broken") },
				"認証の失敗":  func() { e.set("forbidden") },
			},
			Secrets: []string{token},
		}
	})
}

// 値を返すだけで、比べ方は Condition に書く（E-H4・N-T4）。系列が 2 本以上なら
// value は無く、sum・max・min で読む。無いメトリクスを 0 と読まない。
func TestObserve(t *testing.T) {
	e, p := newExporter(t)
	ids := []core.ID{
		series(`mq/rabbitmq_queue_messages{queue="orders"}`),
		series("mq/rabbitmq_queue_messages"),
		series(`mq/rabbitmq_queue_messages{queue="none"}`),
		series(`mq/rabbitmq_queue_messages{vhost=""}`),
		series("other/up"),
		series("mq/bad name"),
	}
	o, err := p.Observe(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if f := o[ids[0]].Fields; o[ids[0]].Availability != core.Present || f["value"] != 42.0 || f["series"] != int64(1) || f["sum"] != 42.0 {
		t.Fatalf("%+v", o[ids[0]])
	}
	if f := o[ids[1]].Fields; f["series"] != int64(2) || f["sum"] != 50.0 || f["max"] != 42.0 || f["min"] != 8.0 {
		t.Fatalf("%+v", o[ids[1]])
	} else if _, ok := f["value"]; ok {
		t.Fatalf("2 本なのに value がある: %+v", f)
	}
	for _, i := range []int{2, 3, 4, 5} {
		if o[ids[i]].Availability != core.Unknown {
			t.Errorf("%s: %+v（want unknown）", ids[i], o[ids[i]])
		}
	}
	e.mu.Lock()
	e.body = "rabbitmq_queue_messages{queue=\"orders\"} NaN\n"
	e.mu.Unlock()
	if o, _ := p.Observe(context.Background(), ids[:1]); o[ids[0]].Availability != core.Unknown {
		t.Fatalf("NaN が %+v", o[ids[0]])
	}
}

func TestValidate(t *testing.T) {
	_, p := newExporter(t)
	for _, c := range []struct {
		r    core.Resource
		want string
	}{
		{core.Resource{ID: series("nope/up")}, "settings.endpoints に無い"},
		{core.Resource{ID: series("mq/up"), Fields: core.Fields{"value": 1.0}}, "fields は書けない"},
		{core.Resource{ID: series(`mq/up{x=1}`)}, "ラベル"},
		{core.Resource{ID: series("up")}, "<endpoint>/"},
	} {
		d := p.Validate([]core.Resource{c.r}, nil)
		if len(d) == 0 || !strings.Contains(d[0].Message, c.want) {
			t.Errorf("%s: %v（want %q）", c.r.ID, d, c.want)
		}
	}
	for _, s := range []map[string]any{
		{"endpoints": map[string]any{"a": map[string]any{"url": "ftp://x/metrics"}}},
		{"endpoints": map[string]any{"a": map[string]any{"url": "http://u:p@x/metrics"}}},
		{"endpoints": map[string]any{"a": map[string]any{"url": "http://x/metrics", "password": "p"}}},
		{"endpoints": map[string]any{"a": map[string]any{"url": "http://x/metrics", "username": "u"}}},
		{"probes": map[string]any{}},
	} {
		if _, err := metricsprobe.Module(s); err == nil {
			t.Errorf("%v: エラーにならなかった", s)
		}
	}
}
