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

package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// CommitGrouper is an OPTIONAL Provider capability, discovered by type
// assertion like ApprovalRequirer (Backlog #199).  It lets a provider put
// several of its resources (e.g. the same change on several network devices)
// in a commit group: the core applies the members that have a diff in one
// Apply call (their ops carry core.Op.CommitGroup), and the provider commits
// them all or none.
//
// The core's part (§23.4):
//
//   - CommitGroup names the group of a resource from its declared fields; ""
//     is none (a resource is never grouped implicitly).  It MUST NOT do I/O.
//   - At load time the core refuses a group whose members depend on each
//     other (directly or not) or lie in more than one scope, and asks the
//     provider with ValidateCommitGroup (e.g. one member per device).
//   - The members are ordered into the same wave (each takes the depends_on
//     of all).  When one member cannot go this cycle (unobservable, held,
//     an upstream failed, backing off, in flight, held by the budget or
//     awaiting approval), none goes: there is no partial approval.
//   - The budget still counts each member's ops as its own change.
//   - Apply returns nil when every member was committed; otherwise a
//     *CommitGroupError saying, per member, committed, not committed or
//     unknown.  An error that is not one fails every member alike.
type CommitGrouper interface {
	CommitGroup(localID string, fields map[string]string) string
	ValidateCommitGroup(group string, localIDs []string) error
}

// CommitGroupOf is the commit group p names for the resource, or "" when p
// is not a CommitGrouper.
func CommitGroupOf(p Provider, localID string, fields map[string]string) string {
	if g, ok := p.(CommitGrouper); ok {
		return g.CommitGroup(localID, fields)
	}
	return ""
}

// CommitOutcome is what became of one member of a commit group.
type CommitOutcome string

const (
	// CommitCommitted: the member's change was committed.
	CommitCommitted CommitOutcome = "committed"
	// CommitNotCommitted: the member's change was not committed (it never
	// was, or the target rolls it back).
	CommitNotCommitted CommitOutcome = "not_committed"
	// CommitUnknown: whether the member's change was committed is not known
	// (e.g. the connection broke while the confirming commit was being
	// sent); the next cycle observes.
	CommitUnknown CommitOutcome = "unknown"
)

// CommitMember is the result of one member of a commit group.
type CommitMember struct {
	Outcome CommitOutcome
	// Err is why the member was not committed or is unknown (nil when
	// committed).  Its kind (TransientError / PermanentError) is the
	// member's own.
	Err error
}

// CommitGroupError is the error of a commit group's Apply that did not
// commit every member: the outcome of each member by its local id.
type CommitGroupError struct {
	Group   string
	Members map[string]CommitMember
}

func (e *CommitGroupError) Error() string {
	var committed, unknown []string
	for _, id := range e.sortedIDs() {
		switch e.Members[id].Outcome {
		case CommitCommitted:
			committed = append(committed, id)
		case CommitUnknown:
			unknown = append(unknown, id)
		}
	}
	switch {
	case len(committed) == 0 && len(unknown) == 0:
		return fmt.Sprintf("commit group %q: no member was committed (%d member(s))", e.Group, len(e.Members))
	default:
		return fmt.Sprintf("commit group %q: not all or nothing: committed %s; unknown %s", e.Group, listOrNone(committed), listOrNone(unknown))
	}
}

func listOrNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

func (e *CommitGroupError) sortedIDs() []string {
	ids := make([]string, 0, len(e.Members))
	for id := range e.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// MemberError is the error of the target's op in an Apply that returned
// err: nil when err is nil or a *CommitGroupError that committed the target;
// the member's own error when err is a *CommitGroupError naming the target;
// err itself otherwise.
func MemberError(err error, target core.ID) error {
	if err == nil {
		return nil
	}
	var ge *CommitGroupError
	if !errors.As(err, &ge) {
		return err
	}
	m, ok := ge.Members[target.Local]
	if !ok {
		return err
	}
	if m.Outcome == CommitCommitted {
		return nil
	}
	if m.Err == nil {
		return NewTransientError(fmt.Errorf("commit group %q: %s", ge.Group, m.Outcome))
	}
	return m.Err
}
