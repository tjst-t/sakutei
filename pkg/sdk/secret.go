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
	"context"

	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// SecretBackend is a store of secrets that SecretRefs name by backend
// ("secretref:<backend>/<path>", §20, #188).  A run configures backends in
// the secret_backends: section of --provider-config, each of a type: the
// built-in "vault-kv2" (Vault KV v2 / OpenBao) and "command" (an outside
// command), or one added with pkg/cli.WithSecretBackend.
//
// Generation is read by render phase 1 for every ref the intent writes
// without "#gen<N>" — metadata only, never the value — and written into the
// ref, so a new generation is a diff (cause E) that gets Applied.  When it
// fails, the resource is unknown that cycle (not Observed or Applied, never
// pruned, §20.3).  Resolve is called only at Apply time, by the provider,
// with the generation of the ref.  An implementation must never put a value
// in an error.
type SecretBackend interface {
	// Generation returns the current generation (a positive integer) of the
	// secret at path.
	Generation(ctx context.Context, path string) (int64, error)
	// Resolve returns the value of the secret at path at the given
	// generation (the current one when 0).
	Resolve(ctx context.Context, path string, generation int64) (string, error)
}

// SecretBackendFactory builds a SecretBackend from its settings in
// secret_backends: (every key but "type"; values as strings).  An unknown
// setting should be an error.
type SecretBackendFactory func(settings map[string]string) (SecretBackend, error)

// NewFileSecretResolver returns the file SecretBackend (§20.5, §23.6) reading
// the site-local store at path, as a core.SecretResolver: a provider outside
// this module resolves the SecretRef of a secret field with it at Apply time
// (core.ParseSecretRef, then SecretRef.Resolve; Zero the value after use).
// The store is the same file `sakutei secrets extract` writes.
//
// A ref naming a backend configured in secret_backends: (#188) is resolved by
// that backend, so a provider using this resolver takes Vault refs too.
// path may be empty when the provider has no file store.
func NewFileSecretResolver(path string) core.SecretResolver {
	return render.NewFileSecretBackend(path)
}
