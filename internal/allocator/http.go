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

package allocator

// The allocator as a service: the HA minimal form of §19.8.  Two (or more)
// `sakutei allocator` processes share one store (the S2 location, e.g. on the
// stage-0 VM-HA / Ceph); one is active, the others stand by.  Taking over is
// Promote: the store's fencing generation is incremented, so a former active
// that is still alive is fenced on its next request and steps down — two
// authorities never hand out values from one pool (§19.7).  Clients list
// every endpoint and fail over to whichever answers as active.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/allocation"
)

// Error codes on the wire.
const (
	codeUnknownPool   = "unknown_pool"
	codeExhausted     = "pool_exhausted"
	codeNoReservation = "no_reservation"
	codeFenced        = "fenced"
	codeStandby       = "standby"
	codeInternal      = "internal"
)

type request struct {
	Pool       string  `json:"pool,omitempty"`
	Owner      string  `json:"owner,omitempty"`
	TTLSeconds float64 `json:"ttl_seconds,omitempty"`
	Lo         int     `json:"lo,omitempty"`
	Hi         int     `json:"hi,omitempty"`
}

type response struct {
	Value      string `json:"value,omitempty"`
	OK         bool   `json:"ok,omitempty"`
	Role       string `json:"role,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	Error      string `json:"error,omitempty"`
	Code       string `json:"code,omitempty"`
	// Records answers "list" (#116).
	Records []Record `json:"records,omitempty"`
}

// Server serves a FileAuthority over HTTP as one member of an active/standby
// pair.
type Server struct {
	auth *FileAuthority

	mu     sync.Mutex
	active bool
}

// NewServer returns a server for auth.  A standby server answers every
// operation with 503 until it is promoted (POST /v1/promote).
func NewServer(auth *FileAuthority) *Server {
	return &Server{auth: auth}
}

// Promote makes the server active, fencing every earlier generation.
func (s *Server) Promote() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gen, err := s.auth.Promote()
	if err != nil {
		return 0, err
	}
	s.active = true
	return gen, nil
}

// Active reports whether the server currently acts as the authority.
func (s *Server) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/")
	if op == "health" {
		role := "standby"
		// An instance fenced by a newer promotion steps down here too, not
		// only on its next operation: health must never show two actives.
		if s.Active() && !errors.Is(s.stepDownIfFenced(s.auth.CheckActive()), allocation.ErrFenced) {
			role = "active"
		}
		writeJSON(w, http.StatusOK, response{Role: role, Generation: s.auth.Generation()})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, response{Error: "POST only", Code: codeInternal})
		return
	}
	if op == "promote" {
		gen, err := s.Promote()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response{Role: "active", Generation: gen})
		return
	}
	if !s.Active() {
		writeJSON(w, http.StatusServiceUnavailable, response{Error: "allocator is standby", Code: codeStandby})
		return
	}
	// A newer generation may have been promoted elsewhere: step down rather
	// than answer as a second authority (reads included).
	if err := s.stepDownIfFenced(s.auth.CheckActive()); err != nil {
		writeErr(w, err)
		return
	}
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{Error: err.Error(), Code: codeInternal})
		return
	}
	ctx := r.Context()
	var resp response
	var err error
	switch op {
	case "allocate":
		resp.Value, err = s.auth.Allocate(ctx, req.Pool, req.Owner)
	case "reserve":
		resp.Value, err = s.auth.Reserve(ctx, req.Pool, req.Owner, time.Duration(req.TTLSeconds*float64(time.Second)))
	case "confirm":
		err = s.auth.Confirm(ctx, req.Pool, req.Owner)
	case "release":
		// OK says whether the owner held a value (#116); Release of an
		// owner that holds nothing stays a no-op.
		resp.Value, resp.OK, err = s.auth.ReleaseOwner(ctx, req.Pool, req.Owner)
	case "list":
		// Read only, like lookup: the records of req.Pool (all when empty).
		resp.Records, err = s.auth.List(ctx, req.Pool)
	case "lookup":
		resp.Value, resp.OK, err = s.auth.Lookup(ctx, req.Pool, req.Owner)
	case "ensure-pool":
		err = s.auth.EnsurePool(req.Pool, req.Lo, req.Hi)
	default:
		writeJSON(w, http.StatusNotFound, response{Error: "unknown operation " + op, Code: codeInternal})
		return
	}
	if err := s.stepDownIfFenced(err); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// stepDownIfFenced turns the server standby when err says a newer generation
// was promoted, and returns err unchanged.
func (s *Server) stepDownIfFenced(err error) error {
	if errors.Is(err, allocation.ErrFenced) {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
	}
	return err
}

func writeErr(w http.ResponseWriter, err error) {
	code, status := codeInternal, http.StatusInternalServerError
	switch {
	case errors.Is(err, allocation.ErrUnknownPool):
		code, status = codeUnknownPool, http.StatusNotFound
	case errors.Is(err, allocation.ErrPoolExhausted):
		code, status = codeExhausted, http.StatusConflict
	case errors.Is(err, allocation.ErrNoReservation):
		code, status = codeNoReservation, http.StatusConflict
	case errors.Is(err, allocation.ErrFenced):
		code, status = codeFenced, http.StatusServiceUnavailable
	}
	writeJSON(w, status, response{Error: err.Error(), Code: code})
}

func writeJSON(w http.ResponseWriter, status int, v response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// DefaultClientTimeout bounds one request to one endpoint.
const DefaultClientTimeout = 2 * time.Second

// Client is the AllocationAuthority of a remote allocator pair.  Each call
// goes to the endpoint that answered last and fails over to the others when
// one is down, standby or fenced.  When none answers as active the call
// fails with allocation.ErrUnavailable, which render turns into
// waiting-allocation for the requesting resource (§19.9).
type Client struct {
	endpoints []string
	http      *http.Client

	mu        sync.Mutex
	preferred int
}

var _ allocation.AllocationAuthority = (*Client)(nil)

// NewClient returns a client for the given base URLs (e.g.
// "http://alloc-a:7070").
func NewClient(endpoints []string) *Client {
	eps := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		if e = strings.TrimRight(strings.TrimSpace(e), "/"); e != "" {
			eps = append(eps, e)
		}
	}
	return &Client{endpoints: eps, http: &http.Client{Timeout: DefaultClientTimeout}}
}

// call runs op on the active endpoint.  Every operation is idempotent
// (Allocate per owner, Release of a released owner is a no-op), so retrying it
// on the next endpoint is safe.
func (c *Client) call(ctx context.Context, op string, req request) (response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return response{}, err
	}
	c.mu.Lock()
	start := c.preferred
	c.mu.Unlock()
	var lastErr error
	for i := range c.endpoints {
		idx := (start + i) % len(c.endpoints)
		resp, err := c.post(ctx, c.endpoints[idx]+"/v1/"+op, body)
		if err != nil {
			lastErr = err
			continue
		}
		switch resp.Code {
		case "":
			c.mu.Lock()
			c.preferred = idx
			c.mu.Unlock()
			return resp, nil
		case codeStandby, codeFenced:
			lastErr = fmt.Errorf("%s: %s", c.endpoints[idx], resp.Error)
			continue
		case codeUnknownPool:
			return resp, fmt.Errorf("%w: %s", allocation.ErrUnknownPool, resp.Error)
		case codeExhausted:
			return resp, fmt.Errorf("%w: %s", allocation.ErrPoolExhausted, resp.Error)
		case codeNoReservation:
			return resp, fmt.Errorf("%w: %s", allocation.ErrNoReservation, resp.Error)
		default:
			return resp, fmt.Errorf("allocator %s: %s", c.endpoints[idx], resp.Error)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no endpoint configured")
	}
	return response{}, fmt.Errorf("%w: %v", allocation.ErrUnavailable, lastErr)
}

func (c *Client) post(ctx context.Context, url string, body []byte) (response, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(hr)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	var resp response
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&resp); err != nil {
		return response{}, fmt.Errorf("%s: HTTP %d: %w", url, res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK && resp.Code == "" {
		resp.Code = codeInternal
		resp.Error = fmt.Sprintf("HTTP %d", res.StatusCode)
	}
	return resp, nil
}

// Allocate implements allocation.AllocationAuthority.
func (c *Client) Allocate(ctx context.Context, pool, owner string) (string, error) {
	resp, err := c.call(ctx, "allocate", request{Pool: pool, Owner: owner})
	return resp.Value, err
}

// Reserve implements allocation.AllocationAuthority.
func (c *Client) Reserve(ctx context.Context, pool, owner string, ttl time.Duration) (string, error) {
	resp, err := c.call(ctx, "reserve", request{Pool: pool, Owner: owner, TTLSeconds: ttl.Seconds()})
	return resp.Value, err
}

// Confirm implements allocation.AllocationAuthority.
func (c *Client) Confirm(ctx context.Context, pool, owner string) error {
	_, err := c.call(ctx, "confirm", request{Pool: pool, Owner: owner})
	return err
}

// Release implements allocation.AllocationAuthority.
func (c *Client) Release(ctx context.Context, pool, owner string) error {
	_, err := c.call(ctx, "release", request{Pool: pool, Owner: owner})
	return err
}

// ReleaseOwner is Release that also reports the released value and whether
// the owner held one (#116).
func (c *Client) ReleaseOwner(ctx context.Context, pool, owner string) (string, bool, error) {
	resp, err := c.call(ctx, "release", request{Pool: pool, Owner: owner})
	return resp.Value, resp.OK, err
}

// List returns the records of pool (every pool when empty) from the active
// allocator (#116).  It never writes.
func (c *Client) List(ctx context.Context, pool string) ([]Record, error) {
	resp, err := c.call(ctx, "list", request{Pool: pool})
	if resp.Records == nil {
		resp.Records = []Record{}
	}
	return resp.Records, err
}

// Lookup implements allocation.AllocationAuthority.
func (c *Client) Lookup(ctx context.Context, pool, owner string) (string, bool, error) {
	resp, err := c.call(ctx, "lookup", request{Pool: pool, Owner: owner})
	return resp.Value, resp.OK, err
}

// EnsurePool declares a pool to the active allocator (intent `pools:`, §19.8).
func (c *Client) EnsurePool(name string, lo, hi int) error {
	_, err := c.call(context.Background(), "ensure-pool", request{Pool: name, Lo: lo, Hi: hi})
	return err
}
