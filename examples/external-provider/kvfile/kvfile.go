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

// Package kvfile is a provider written outside the sakutei module: it uses
// only pkg/ (provider, core, sdk).  It keeps records ("name" → "value") in
// one JSON file.
//
// Config keys (from --provider-config, laid over the flag-derived config):
//
//	path          the JSON file (default: the --state value, then kvfile.json)
//	secret_store  the file SecretBackend store; required to Apply a "token"
//	              (a secret field: only its SecretRef is in intent, and only a
//	              digest of the resolved value is written to the file)
package kvfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

const (
	// Name is the provider name intent resources use.
	Name = "kvfile"
	// ProviderVersion is the diff / normalisation version (§10.4).
	ProviderVersion = Name + "-v1"
)

// record is one entry of the file.  Token is the ref form of the secret
// field; TokenDigest proves the value was resolved at Apply time.
type record struct {
	Value       string `json:"value"`
	Token       string `json:"token,omitempty"`
	TokenDigest string `json:"token_sha256,omitempty"`
}

// Provider implements provider.Provider and provider.Lister.
type Provider struct {
	path    string
	secrets core.SecretResolver
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Lister   = (*Provider)(nil)
)

// New builds the provider from its config map.
func New(config map[string]string) (*Provider, error) {
	path := config["path"]
	if path == "" {
		path = config["state"]
	}
	if path == "" {
		path = "kvfile.json"
	}
	p := &Provider{path: path}
	if s := config["secret_store"]; s != "" {
		p.secrets = sdk.NewFileSecretResolver(s)
	}
	return p, nil
}

// Schema declares the fields (§28.4).
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		"name":  {Kind: core.FieldKindString, Required: true, Identity: true},
		"value": {Kind: core.FieldKindString, Required: true},
		"token": {Kind: core.FieldKindString, Secret: true},
	}}
}

// Capabilities: values are observable and the file can be listed in full.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, List: true, ProviderVersion: ProviderVersion}
}

// Read observes one record.  A missing file is an empty file (absent), an
// unreadable one is unknown.
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	recs, err := p.load()
	if err != nil {
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()},
			provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	obs := core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}
	if r, ok := recs[id]; ok {
		obs.State = toState(id, r)
	}
	return obs, nil
}

// Apply applies the plan to the file (idempotent).
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	recs, err := p.load()
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("%s: Apply: %w", Name, err))
	}
	for _, op := range plan.Ops {
		name := op.Target.Local
		if op.Kind == core.OpKindDelete {
			delete(recs, name)
			continue
		}
		r := recs[name]
		for _, f := range op.Fields {
			switch f.Field {
			case "value":
				r.Value = f.To
			case "token":
				if err := p.setToken(ctx, &r, f.To); err != nil {
					return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %w", Name, name, err))
				}
			}
		}
		recs[name] = r
	}
	if err := p.save(recs); err != nil {
		return provider.NewTransientError(fmt.Errorf("%s: Apply: %w", Name, err))
	}
	return nil
}

// setToken resolves the SecretRef just in time (§20.2) and keeps only the
// ref and a digest of the value.
func (p *Provider) setToken(ctx context.Context, r *record, v string) error {
	if v == "" {
		r.Token, r.TokenDigest = "", ""
		return nil
	}
	ref, err := core.ParseSecretRef(v)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	if p.secrets == nil {
		return errors.New("token: secret_store is not set in --provider-config")
	}
	val, err := ref.Resolve(ctx, p.secrets)
	if err != nil {
		return fmt.Errorf("token %s: %w", ref, err)
	}
	sum := sha256.Sum256([]byte(val.Expose()))
	val.Zero()
	// Keep the ref as intent wrote it, so the next Read compares equal.
	r.Token, r.TokenDigest = v, hex.EncodeToString(sum[:])
	return nil
}

// List returns every record of the file (complete-or-error, §5.4).
func (p *Provider) List(ctx context.Context, scope core.Scope) (core.ListResult, error) {
	recs, err := p.load()
	if err != nil {
		return core.ListResult{Status: core.ListUnknown},
			provider.NewTransientError(fmt.Errorf("%s: List: %w", Name, err))
	}
	names := make([]string, 0, len(recs))
	for name := range recs {
		names = append(names, name)
	}
	sort.Strings(names)
	ids := make([]core.ID, 0, len(names))
	for _, name := range names {
		ids = append(ids, core.NewID(Name, scope, name))
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

func toState(name string, r record) core.State {
	s := core.State{
		"name":  {Kind: core.FieldKindString, Value: name},
		"value": {Kind: core.FieldKindString, Value: r.Value},
	}
	if r.Token != "" {
		s["token"] = core.TypedValue{Kind: core.FieldKindString, Value: r.Token}
	}
	return s
}

func (p *Provider) load() (map[string]record, error) {
	data, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]record{}, nil
	}
	if err != nil {
		return nil, err
	}
	recs := map[string]record{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("%s: %w", p.path, err)
	}
	return recs, nil
}

func (p *Provider) save(recs map[string]record) error {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".kvfile-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.path)
}
