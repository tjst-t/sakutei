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

package graphreport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func res(local string, deps ...string) diff.DesiredResource {
	return diff.DesiredResource{ID: core.ID{Provider: "fake", Local: local}, DependsOn: deps}
}

func TestBuild_WavesEdgesAndReadiness(t *testing.T) {
	db := res("db")
	app := res("app", "db", "ext-lb")
	app.ReadyConditions = map[string]string{"db": "accepting", "ext-lb": "healthy"}
	web := res("web", "app")
	r := New(time.Unix(0, 0))
	r.Build([]diff.DesiredResource{web, app, db}, map[string]map[string]bool{"db": {"accepting": true}})

	if w, _ := json.Marshal(r.Waves); string(w) != `[["db"],["app"],["web"]]` {
		t.Errorf("waves %s", w)
	}
	byLocal := map[string]Node{}
	for _, n := range r.Nodes {
		byLocal[n.LocalID] = n
	}
	if n := byLocal["ext-lb"]; !n.External || n.Wave != -1 || n.Observed {
		t.Errorf("ext-lb %+v, want an unobserved external upstream", n)
	}
	if n := byLocal["app"]; n.Ready || n.WaitingFor != "ext-lb:healthy" || n.Wave != 1 {
		t.Errorf("app %+v, want not ready waiting for ext-lb:healthy (unknown ≠ ready)", n)
	}
	if n := byLocal["web"]; !n.Ready {
		t.Errorf("web %+v: an order-only dependency does not make it unready", n)
	}
	if r.Summary.Nodes != 4 || r.Summary.External != 1 || r.Summary.Edges != 3 || r.Summary.NotReady != 1 {
		t.Errorf("summary %+v", r.Summary)
	}
	for _, e := range r.Edges {
		switch e.From + ">" + e.To {
		case "db>app":
			if e.Ready == nil || !*e.Ready {
				t.Errorf("db>app %+v, want ready", e)
			}
		case "app>web":
			if e.Ready != nil {
				t.Errorf("app>web %+v has no ready_condition", e)
			}
		}
	}
}
