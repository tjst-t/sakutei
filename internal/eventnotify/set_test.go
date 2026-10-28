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

package eventnotify

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func TestSplitURLs(t *testing.T) {
	got := SplitURLs([]string{"http://a:9093, http://b:9093", "", "http://c:9093,"})
	want := []string{"http://a:9093", "http://b:9093", "http://c:9093"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SplitURLs = %v, want %v", got, want)
	}
}

func TestLoadConfig_URLs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notify.yaml")
	write := func(s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("webhook:\n  urls:\n    - https://hooks.example/a\nalertmanager:\n  urls:\n    - http://am-1:9093\n    - http://am-2:9093\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Webhook.URLs, []string{"https://hooks.example/a"}) ||
		!reflect.DeepEqual(cfg.Alertmanager.URLs, []string{"http://am-1:9093", "http://am-2:9093"}) {
		t.Errorf("cfg = %+v", cfg)
	}
	for _, tc := range []struct{ in, want string }{
		{"alertmanager:\n  urls: http://am-1:9093\n", "want a list"},
		{"alertmanager:\n  urls:\n    - am-1:9093\n", "alertmanager.urls[0]"},
	} {
		write(tc.in)
		if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoadConfig(%q) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

func TestOpen_EveryDestinationOnItsOwn(t *testing.T) {
	up, down, conf := &recv{}, &recv{}, &recv{}
	down.down.Store(true)
	s1, s2, s3 := httptest.NewServer(up), httptest.NewServer(down), httptest.NewServer(conf)
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	cfg := Config{Alertmanager: Target{ResendInterval: time.Hour, URLs: []string{s3.URL, s1.URL + "/"}}}
	// s1 twice (the flag and the config, one with a trailing "/"): one sink.
	set, err := Open(cfg, nil, []string{s1.URL, s2.URL}, fastOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close(context.Background())
	if set.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (s1, s2, s3)", set.Len())
	}
	for _, sk := range set.Sinks() {
		_ = sk.Emit(context.Background(), driftEvent(core.EventFiring))
	}
	waitFor(t, "the firing alert at both up destinations, failures at the down one", func() bool {
		st := set.Status()
		return up.count() >= 1 && conf.count() >= 1 && st[1].Failed >= 2
	})
	st := set.Status()
	if st[0].URL != s1.URL || st[0].Sink != "alertmanager" || st[0].Delivered < 1 || st[0].Down() || st[0].LastSuccess == nil || st[0].Active != 1 {
		t.Errorf("up destination status = %+v", st[0])
	}
	if st[1].URL != s2.URL || !st[1].Down() || st[1].Delivered != 0 || !strings.Contains(st[1].LastError, "503") || st[1].LastFailure == nil {
		t.Errorf("down destination status = %+v", st[1])
	}
	down.down.Store(false)
	waitFor(t, "the firing alert at the recovered destination", func() bool { return down.count() >= 1 && !set.Status()[1].Down() })
}

func TestOpen_BadURLFails(t *testing.T) {
	if _, err := Open(Config{}, nil, []string{"http://ok:9093", "am:9093"}, fastOpts); err == nil {
		t.Error("Open with a bad URL = nil error")
	}
}

func TestSetClose_DownDestinationDoesNotStarveTheOthers(t *testing.T) {
	up, down := &recv{}, &recv{}
	down.down.Store(true)
	s1, s2 := httptest.NewServer(up), httptest.NewServer(down)
	defer s1.Close()
	defer s2.Close()
	set, err := Open(Config{}, []string{s2.URL, s1.URL}, nil, fastOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range set.Sinks() {
		_ = sk.Emit(context.Background(), driftEvent(core.EventFiring))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = set.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), "not delivered before shutdown") || strings.Contains(err.Error(), s1.URL) {
		t.Errorf("Close = %v, want only the down destination's loss", err)
	}
	if up.count() != 1 {
		t.Errorf("the up webhook got %d Events, want 1", up.count())
	}
}
