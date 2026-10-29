// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reconcile

import (
	"fmt"
	"io"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/runtimeargs"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// RuntimeReport is what the daemon reports, every cycle, about the intent's
// runtime: (§26.7, Backlog #183).  The start arguments are decided when the
// daemon starts (the flags checked against runtime:); a cycle then raises
//
//   - RuntimeTightened (level) while a flag tightens the declaration (a
//     smaller budget, a weaker mode), with the flag's value and the
//     intent's, and
//   - RuntimeDeclarationChanged (level) while the intent's runtime: differs
//     from the one the daemon started with: it keeps the values it started
//     with until it is restarted.
//
// Both are also in the daemon status (DaemonHealth).
type RuntimeReport struct {
	// Start is the runtime: the daemon started with, as the daemon takes it
	// (runtimeargs.Declaration.For(Daemon); nil: none declared).
	Start *runtimeargs.Declaration
	// Unread means the intent could not be read at start (an --intent-repo
	// not pulled yet, an intent that does not load): the daemon started on
	// the flags alone, and any runtime: it later reads is a change.
	Unread bool
	// Tightened are the flags tightening Start.
	Tightened []runtimeargs.Tightening

	// changed is the latest cycle's account of a changed runtime: ("" when
	// none); checked the cycle (Daemon.cycle) it was last compared on.
	changed string
	checked int
}

// EmitRuntimeTightened raises one RuntimeTightened Event for ts (none when
// empty).  The one-shot reconcile uses it too.
func EmitRuntimeTightened(w io.Writer, prefix string, ts []runtimeargs.Tightening) {
	if len(ts) == 0 {
		return
	}
	msg := runtimeargs.TighteningText(ts)
	eventlog.Emit(w, core.Event{
		Kind: core.EventRuntimeTightened, Severity: core.SeverityWarning, Message: msg,
		Attrs: runtimeargs.TighteningAttrs(ts),
		Text:  prefix + "RuntimeTightened: " + msg,
	})
}

// observe raises the cycle's runtime Events for the runtime: decl just
// loaded.
func (r *RuntimeReport) observe(w io.Writer, decl *runtimeargs.Declaration, cycle int) {
	r.checked = cycle
	EmitRuntimeTightened(w, "sakutei: ", r.Tightened)
	var changes []runtimeargs.Change
	if r.Unread {
		changes = (*runtimeargs.Declaration)(nil).Changes(decl)
	} else {
		changes = r.Start.Changes(decl)
	}
	if len(changes) == 0 {
		r.changed = ""
		return
	}
	attrs := map[string]string{}
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		from, to := c.From, c.To
		if r.Unread {
			from = "(intent not read at start)"
		} else if from == "" {
			from = "(not declared)"
		}
		if to == "" {
			to = "(not declared)"
		}
		attrs[c.Key+".started"], attrs[c.Key+".intent"] = from, to
		parts = append(parts, fmt.Sprintf("%s: started with %s, now %s", c.Key, from, to))
	}
	r.changed = strings.Join(parts, "; ")
	msg := "the intent's runtime: changed since sakutei started (" + r.changed +
		"); this daemon keeps the values it started with — restart it to take the declaration in (§26.7)"
	eventlog.Emit(w, core.Event{
		Kind: core.EventRuntimeDeclarationChanged, Severity: core.SeverityWarning, Message: msg,
		Attrs: attrs,
		Text:  "sakutei: RuntimeDeclarationChanged: " + msg,
	})
}

// resolves reports whether a runtime Event that did not recur on cycle is
// gone: the runtime: was compared on that cycle.
func (r *RuntimeReport) resolves(cycle int) bool {
	return r != nil && r.checked == cycle
}
