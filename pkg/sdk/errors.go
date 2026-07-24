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

package sdk

import (
	"errors"

	"github.com/tjst-t/sakutei/pkg/provider"
)

// IsTransient reports whether err (or any error in its chain) is a
// TransientError — an error expected to resolve on its own with time.
//
// The controller uses this to decide whether to back off and retry (transient)
// or to move the resource to "stuck" until desired state or provider_version
// changes (permanent).
//
// Usage:
//
//	if sdk.IsTransient(err) {
//	    // schedule retry with exponential back-off
//	}
func IsTransient(err error) bool {
	var te *provider.TransientError
	return errors.As(err, &te)
}

// IsPermanent reports whether err (or any error in its chain) is a
// PermanentError — an error that will not resolve without a change to desired
// state or the provider version.
//
// Usage:
//
//	if sdk.IsPermanent(err) {
//	    // mark resource as stuck; do not retry this cycle
//	}
func IsPermanent(err error) bool {
	var pe *provider.PermanentError
	return errors.As(err, &pe)
}
