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

// Package dnszone provides a provider.Provider, provider.Lister and
// provider.BulkObserver implementation that manages DNS records in an
// RFC1035-format zone file (§27.2, decisions.json S62dbba).
//
// Design decisions (decisions.json, S62dbba):
//
//   - stdlib only: bufio.Scanner + strings.Fields for RFC1035 subset parsing;
//     no external libraries (miekg/dns etc.) to stay offline-capable.
//   - identity is {name, type}: same FQDN with different record types (A vs CNAME)
//     are distinct resources.  TTL and value are non-identity attributes.
//   - SOA and NS records are explicitly excluded from management (management
//     boundary, §27.2).  Read/Apply/List silently skip them.
//   - Atomic write: Apply uses os.CreateTemp + os.Rename so a crash mid-write
//     never leaves a truncated zone file (S2 progress class, §4.9).
//   - Pruneable: true — DNS records are cheap/reversible to recreate (non-destructive
//     type by the ADR-0005 classification); automatic orphan deletion is safe.
//   - BulkObserve: true — one read of the zone file yields every record, so a
//     cycle reads the file once instead of once per record (§5.7, Backlog #27).
//
// Zone file format supported (RFC1035 subset):
//
//	name TTL IN type value
//	name TTL type value     (CLASS optional)
//	name IN type value      (TTL optional, defaults to 300)
//	name type value         (bare minimum)
//
// Comment lines (;) and blank lines are preserved verbatim on write.
// $ORIGIN and $TTL directives are preserved verbatim (not interpreted).
//
// Local ID convention: "<name>/<type>" (e.g. "www.example.com./A").
// This encodes the composite {name, type} identity into a single opaque string
// that the convergence core passes to Read/Apply.
package dnszone

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// ProviderName is the canonical name for this provider in the registry.
const ProviderName = "dnszone"

// providerVersion is a hard-coded source hash for this provider version.
// In future the SDK will compute this from source; for now it is set manually
// so the fail-closed gate (§10.4) passes on registration.
// Increment this constant whenever normalisation or diff logic changes so that
// the cause-tag C (ghost diff) mechanism can detect provider upgrades.
const providerVersion = "dnszone-v1.0.0"

// managedSkipTypes lists the record types that are NOT managed by this provider.
// SOA and NS are zone-metadata records that named/CoreDNS manages itself;
// writing them would break zone transfers (serial) and server configuration.
var managedSkipTypes = map[string]bool{
	"SOA": true,
	"NS":  true,
}

// record is a parsed DNS resource record from the zone file.
type record struct {
	// name is the owner name (FQDN, e.g. "www.example.com.").
	name string
	// ttl is the time-to-live in seconds (string, preserved as-is).
	ttl string
	// rrtype is the record type (A, AAAA, CNAME, TXT, MX, etc.).
	rrtype string
	// value is the rdata (record data).
	value string
}

// localID returns the provider-local ID for this record: "<name>/<type>".
func (r record) localID() string {
	return r.name + "/" + r.rrtype
}

// Provider manages DNS records in an RFC1035 zone file.
// It implements provider.Provider, provider.Lister and provider.BulkObserver.
type Provider struct {
	// zonePath is the path to the zone file.
	zonePath string
}

// New creates a new dnszone Provider backed by the zone file at zonePath.
// The zone file need not exist yet; Apply will create it.
func New(zonePath string) *Provider {
	return &Provider{zonePath: zonePath}
}

// Schema returns the field declarations for DNS records.
//
// Fields:
//   - name   (string, identity, required): owner name (FQDN).
//   - type   (string, identity, required): record type (A/AAAA/CNAME/TXT/MX/…).
//   - value  (string, required): rdata.
//   - ttl    (string): time-to-live in seconds (default "300" if absent).
//
// Identity is {name, type}: the same FQDN can have both A and CNAME records,
// so type must be part of the identity key (decisions.json S62dbba).
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name": {
				Kind:     core.FieldKindString,
				Required: true,
				Identity: true,
			},
			"type": {
				Kind:     core.FieldKindString,
				Required: true,
				Identity: true,
			},
			"value": {
				Kind:     core.FieldKindString,
				Required: true,
			},
			"ttl": {
				Kind:     core.FieldKindString,
				Required: false,
			},
		},
	}
}

// Capabilities declares this provider's capabilities.
//
// TrueObserve: true — the zone file is the source of truth; Read returns real state.
// List: true — we can enumerate all records in the zone file.
// Pruneable: true — DNS records are cheap/reversible; orphans may be auto-deleted.
// BulkObserve: true — BulkObserve reads the zone file once for every record.
// ProviderVersion: a hard-coded source hash for fail-closed ghost-diff detection.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		Pruneable:       true,
		BulkObserve:     true,
		ProviderVersion: providerVersion,
	}
}

// Read observes the current state of the DNS record identified by localID.
//
// localID format: "<name>/<type>" (e.g. "www.example.com./A").
//
// Constraints (ADR-0002):
//   - Does NOT consult desired state.
//   - If the zone file does not exist, the record is absent (Ok, nil State).
//   - If the zone file cannot be parsed, returns AvailabilityUnknown (transient).
//   - SOA/NS records are never returned (management boundary).
func (p *Provider) Read(_ context.Context, localID string) (core.Observation, error) {
	name, rrtype, err := splitLocalID(localID)
	if err != nil {
		return core.Observation{
			Availability: core.AvailabilityUnknown,
			ObservedAt:   time.Now(),
		}, provider.NewPermanentError(fmt.Errorf("dnszone: Read: invalid local id %q: %w", localID, err))
	}

	// Managed skip: SOA/NS are never managed.
	if managedSkipTypes[strings.ToUpper(rrtype)] {
		return core.Observation{
			Availability: core.AvailabilityOk,
			State:        nil, // absent from management
			ObservedAt:   time.Now(),
		}, nil
	}

	records, err := p.parseZone()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Zone file does not exist → record is absent.
			return core.Observation{
				Availability: core.AvailabilityOk,
				State:        nil,
				ObservedAt:   time.Now(),
			}, nil
		}
		// Parse or read error → unknown (transient: file may be locked or mid-write).
		return core.Observation{
			Availability: core.AvailabilityUnknown,
			ObservedAt:   time.Now(),
		}, provider.NewTransientError(fmt.Errorf("dnszone: Read: parse zone %s: %w", p.zonePath, err))
	}

	// Find the matching record by {name, type}.
	normName := normaliseName(name)
	normType := strings.ToUpper(rrtype)
	for _, r := range records {
		if normaliseName(r.name) == normName && strings.ToUpper(r.rrtype) == normType {
			st := core.State{
				"name":  {Kind: core.FieldKindString, Value: r.name},
				"type":  {Kind: core.FieldKindString, Value: r.rrtype},
				"value": {Kind: core.FieldKindString, Value: r.value},
				"ttl":   {Kind: core.FieldKindString, Value: r.ttl},
			}
			return core.Observation{
				Availability: core.AvailabilityOk,
				State:        st,
				ObservedAt:   time.Now(),
			}, nil
		}
	}

	// Record not found in zone file → absent.
	return core.Observation{
		Availability: core.AvailabilityOk,
		State:        nil,
		ObservedAt:   time.Now(),
	}, nil
}

// Apply converges the DNS record to the desired state described in plan.
//
// Handles Create, Update, and Delete ops.  SOA/NS records are never modified.
// Writes are atomic: a temp file is written then os.Rename'd over the target
// so a crash mid-write never leaves a truncated zone file (decisions.json).
//
// For Create/Update: the record is inserted or replaced in the zone file.
// Existing lines for the same {name, type} are removed; the new line is
// appended at the end (outside managed skip types).
// For Delete: lines matching {name, type} are removed.
//
// MUST be idempotent: Apply(same plan) twice yields the same file content.
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(op); err != nil {
			return err
		}
	}
	return nil
}

// applyOp applies a single Op to the zone file.
func (p *Provider) applyOp(op core.Op) error {
	localID := op.Target.Local
	name, rrtype, err := splitLocalID(localID)
	if err != nil {
		return provider.NewPermanentError(fmt.Errorf("dnszone: Apply: invalid local id %q: %w", localID, err))
	}

	// Guard: never touch SOA/NS.
	if managedSkipTypes[strings.ToUpper(rrtype)] {
		return provider.NewPermanentError(fmt.Errorf("dnszone: Apply: refusing to manage SOA/NS record %q (management boundary)", localID))
	}

	// Extract desired field values from FieldDiffs.
	desired := make(map[string]string, len(op.Fields))
	for _, fd := range op.Fields {
		desired[fd.Field] = fd.To
	}

	// For Update ops, we also need the current "To" values (the desired target).
	// The FieldDiff.To field holds the new desired value; for Create all fields
	// have From=="" and To=desired.

	switch op.Kind {
	case core.OpKindCreate, core.OpKindUpdate:
		// Resolve field values from FieldDiffs.
		recName := name
		recType := rrtype
		recValue := desired["value"]
		recTTL := desired["ttl"]
		if recTTL == "" {
			recTTL = "300"
		}
		// For Update, some fields may not appear in FieldDiffs (only changed fields).
		// Re-read to get the current values for unchanged fields.
		if op.Kind == core.OpKindUpdate {
			if cur, rerr := p.readRecord(name, rrtype); rerr == nil && cur != nil {
				if _, ok := desired["value"]; !ok {
					recValue = cur.value
				}
				if _, ok := desired["ttl"]; !ok {
					recTTL = cur.ttl
				}
			}
		}
		newRec := record{
			name:   recName,
			ttl:    recTTL,
			rrtype: recType,
			value:  recValue,
		}
		return p.writeRecord(newRec)

	case core.OpKindDelete:
		return p.deleteRecord(name, rrtype)

	default:
		return provider.NewPermanentError(fmt.Errorf("dnszone: Apply: unknown op kind %v for %s", op.Kind, localID))
	}
}

// List returns all managed DNS record local IDs in the zone file.
//
// Completeness contract: reads the entire zone file and returns all records
// except SOA and NS.  If the zone file does not exist, returns ListComplete
// with an empty list (reality is empty).  If parsing fails, returns an error
// (never returns a truncated ListComplete — complete-or-error, §5.4).
func (p *Provider) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	records, err := p.parseZone()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Zone file absent → empty list (ListComplete, no records to prune).
			return core.ListResult{IDs: nil, Status: core.ListComplete}, nil
		}
		// Parse error → fail with error (never return truncated ListComplete).
		return core.ListResult{}, provider.NewTransientError(
			fmt.Errorf("dnszone: List: parse zone %s: %w", p.zonePath, err))
	}

	// Use empty scope (provisional — v1a uses root scope).
	scope := core.NewScope(nil)
	ids := make([]core.ID, 0, len(records))
	for _, r := range records {
		ids = append(ids, core.NewID(ProviderName, scope, r.localID()))
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve observes every managed record in the zone file with ONE read of
// the file (§5.7).  Each entry is exactly what Read returns for that record's
// local id, keyed like List (root scope, "<name>/<type>").
//
// Read finds the FIRST record whose normalised {name, type} matches, so every
// entry is built from the first such record — two lines that differ only in
// the case of the name both get the first line's state, as Read gives them.
//
// complete-or-error: a zone file that cannot be read or scanned is an error for
// the whole call (never a partial result).  A missing zone file is an empty
// reality: no entries, no error — each resource then falls back to Read, which
// reports it absent.  The result says nothing about what does not exist and is
// never a prune input (it is not a core.ListResult, ADR-0011).
func (p *Provider) BulkObserve(_ context.Context, _ core.Scope) (provider.BulkObservation, error) {
	records, err := p.parseZone()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return provider.BulkObservation{Observations: map[core.IDKey]core.Observation{}}, nil
		}
		return provider.BulkObservation{}, provider.NewTransientError(
			fmt.Errorf("dnszone: BulkObserve: parse zone %s: %w", p.zonePath, err))
	}

	now := time.Now()
	first := make(map[string]record, len(records))
	for _, r := range records {
		k := normaliseName(r.name) + "/" + strings.ToUpper(r.rrtype)
		if _, ok := first[k]; !ok {
			first[k] = r
		}
	}
	scope := core.NewScope(nil)
	obs := make(map[core.IDKey]core.Observation, len(records))
	for _, rec := range records {
		r := first[normaliseName(rec.name)+"/"+strings.ToUpper(rec.rrtype)]
		obs[core.NewID(ProviderName, scope, rec.localID()).Key()] = core.Observation{
			Availability: core.AvailabilityOk,
			State: core.State{
				"name":  {Kind: core.FieldKindString, Value: r.name},
				"type":  {Kind: core.FieldKindString, Value: r.rrtype},
				"value": {Kind: core.FieldKindString, Value: r.value},
				"ttl":   {Kind: core.FieldKindString, Value: r.ttl},
			},
			ObservedAt: now,
		}
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// Compile-time assertions.
var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.Lister       = (*Provider)(nil)
	_ provider.BulkObserver = (*Provider)(nil)
)

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// splitLocalID splits a local ID of the form "<name>/<type>" into its two parts.
// The name may itself contain "/" if the FQDN has slashes, but in practice DNS
// names do not, so we split on the LAST "/" to allow the type to be at the end.
func splitLocalID(localID string) (name, rrtype string, err error) {
	idx := strings.LastIndex(localID, "/")
	if idx < 0 || idx == 0 || idx == len(localID)-1 {
		return "", "", fmt.Errorf("expected '<name>/<type>' format")
	}
	return localID[:idx], localID[idx+1:], nil
}

// normaliseName canonicalises a DNS name for comparison.
// Trailing dot is always added; name is lowercased for case-insensitive match.
func normaliseName(name string) string {
	n := strings.ToLower(name)
	if !strings.HasSuffix(n, ".") {
		n += "."
	}
	return n
}

// readRecord reads a single record from the zone file by {name, type}.
// Returns nil if not found or if the zone file does not exist.
func (p *Provider) readRecord(name, rrtype string) (*record, error) {
	records, err := p.parseZone()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	normName := normaliseName(name)
	normType := strings.ToUpper(rrtype)
	for i, r := range records {
		if normaliseName(r.name) == normName && strings.ToUpper(r.rrtype) == normType {
			return &records[i], nil
		}
	}
	return nil, nil
}

// writeRecord inserts or replaces a record in the zone file (Create/Update).
// Existing lines for the same {name, type} are removed; the new line is
// appended.  Atomic write via temp file + rename.
func (p *Provider) writeRecord(rec record) error {
	lines, err := p.readLines()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return provider.NewTransientError(fmt.Errorf("dnszone: writeRecord: read zone: %w", err))
	}

	normName := normaliseName(rec.name)
	normType := strings.ToUpper(rec.rrtype)

	// Remove existing lines for this {name, type}.
	filtered := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		r, ok := parseLine(line)
		if !ok {
			// Not a resource record (comment, directive, blank) — preserve.
			filtered = append(filtered, line)
			continue
		}
		if normaliseName(r.name) == normName && strings.ToUpper(r.rrtype) == normType {
			// This is the record we are replacing — skip (drop) it.
			continue
		}
		filtered = append(filtered, line)
	}

	// Append the new record.
	newLine := formatRecord(rec)
	filtered = append(filtered, newLine)

	return p.writeLines(filtered)
}

// deleteRecord removes all lines for {name, type} from the zone file.
// Atomic write via temp file + rename.
func (p *Provider) deleteRecord(name, rrtype string) error {
	lines, err := p.readLines()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Zone file does not exist; deletion is a no-op (idempotent).
			return nil
		}
		return provider.NewTransientError(fmt.Errorf("dnszone: deleteRecord: read zone: %w", err))
	}

	normName := normaliseName(name)
	normType := strings.ToUpper(rrtype)

	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		r, ok := parseLine(line)
		if ok && normaliseName(r.name) == normName && strings.ToUpper(r.rrtype) == normType {
			// Drop this line.
			continue
		}
		filtered = append(filtered, line)
	}

	return p.writeLines(filtered)
}

// parseZone reads the zone file and returns all managed records.
// SOA and NS records are excluded (management boundary).
// Returns os.ErrNotExist if the zone file does not exist.
func (p *Provider) parseZone() ([]record, error) {
	lines, err := p.readLines()
	if err != nil {
		return nil, err
	}

	var records []record
	for _, line := range lines {
		r, ok := parseLine(line)
		if !ok {
			continue
		}
		if managedSkipTypes[strings.ToUpper(r.rrtype)] {
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

// readLines reads the zone file and returns its lines (without trailing newline).
// Returns (nil, os.ErrNotExist) if the file does not exist.
func (p *Provider) readLines() ([]string, error) {
	f, err := os.Open(p.zonePath)
	if err != nil {
		return nil, err // os.ErrNotExist is preserved via errors.Is
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan zone file: %w", err)
	}
	return lines, nil
}

// writeLines writes lines to the zone file atomically (temp + rename).
// Each line is written with a newline terminator.
func (p *Provider) writeLines(lines []string) error {
	dir := filepath.Dir(p.zonePath)
	tmp, err := os.CreateTemp(dir, "sakutei-dnszone-*.tmp")
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: create temp: %w", err))
	}
	tmpPath := tmp.Name()

	w := bufio.NewWriter(tmp)
	for _, line := range lines {
		if _, werr := fmt.Fprintln(w, line); werr != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: write: %w", werr))
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: flush: %w", err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: close temp: %w", err))
	}

	// Atomic rename over the target path.
	if err := os.Rename(tmpPath, p.zonePath); err != nil {
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("dnszone: writeLines: rename: %w", err))
	}
	return nil
}

// parseLine parses a single zone-file line into a record.
// Returns (zero, false) if the line is a comment, blank, or directive.
//
// Supported formats (RFC1035 subset):
//
//	name TTL IN TYPE value...
//	name IN TTL TYPE value...
//	name TTL TYPE value...       (no CLASS)
//	name TYPE value...           (no TTL, no CLASS)
//
// Fields after TYPE are joined as the record value (handles multi-word rdata
// like MX priority + exchange, or quoted TXT strings).
func parseLine(line string) (record, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "$") {
		return record{}, false
	}

	fields := strings.Fields(trimmed)
	if len(fields) < 3 {
		return record{}, false
	}

	// fields[0] is always the owner name.
	name := fields[0]

	// Walk through fields[1..] to find the TYPE token.
	// TYPE is identified by being a known DNS RR type (uppercase alpha, 1-10 chars).
	// TTL is a numeric field. CLASS is "IN" (or CH/HS but we treat all as non-type).
	// Layout possibilities:
	//   [TTL] [CLASS] TYPE rdata...
	//   [CLASS] [TTL] TYPE rdata...  (rare but valid per RFC1035)

	idx := 1
	ttl := "300" // default TTL

	for idx < len(fields)-1 {
		tok := fields[idx]
		if isNumeric(tok) {
			ttl = tok
			idx++
			continue
		}
		upper := strings.ToUpper(tok)
		if upper == "IN" || upper == "CH" || upper == "HS" || upper == "ANY" {
			// CLASS token — skip.
			idx++
			continue
		}
		// Must be TYPE token.
		break
	}

	if idx >= len(fields) {
		return record{}, false
	}

	rrtype := strings.ToUpper(fields[idx])
	idx++

	if idx >= len(fields) {
		// No rdata — malformed.
		return record{}, false
	}

	// Everything after TYPE is the rdata value.
	value := strings.Join(fields[idx:], " ")

	return record{
		name:   name,
		ttl:    ttl,
		rrtype: rrtype,
		value:  value,
	}, true
}

// formatRecord converts a record to a canonical zone-file line.
// Format: "<name> <ttl> IN <type> <value>"
func formatRecord(r record) string {
	ttl := r.ttl
	if ttl == "" {
		ttl = "300"
	}
	return fmt.Sprintf("%s %s IN %s %s", r.name, ttl, strings.ToUpper(r.rrtype), r.value)
}

// isNumeric returns true if s represents a non-negative integer (TTL value).
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}
