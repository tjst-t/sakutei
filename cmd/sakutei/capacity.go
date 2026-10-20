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

package main

import (
	"strings"

	"github.com/tjst-t/sakutei/internal/capacity"
)

// capacityGateUsage is the shared help text of the --capacity-gate flag.
const capacityGateUsage = "capacity gate (§13.4), repeatable: " +
	"resource=<id>,used=<field>,total=<field>,max_utilization=80%[,on_exceed=halt-all|halt-destructive-only][,provider=<name>]; " +
	"halts new changes while the pool's observed utilization is above the threshold (or unknown)"

// capacityGateFlag collects repeated --capacity-gate values (flag.Value).
type capacityGateFlag []capacity.Gate

func (f *capacityGateFlag) String() string {
	parts := make([]string, 0, len(*f))
	for _, g := range *f {
		parts = append(parts, g.Resource)
	}
	return strings.Join(parts, ";")
}

func (f *capacityGateFlag) Set(s string) error {
	g, err := capacity.ParseGate(s)
	if err != nil {
		return err
	}
	*f = append(*f, g)
	return nil
}
