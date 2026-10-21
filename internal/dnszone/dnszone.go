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
// $ORIGIN and $TTL directives are preserved verbatim.  $ORIGIN is interpreted
// (RFC1035 §5.1): an owner name not ending in "." is relative to the $ORIGIN
// in effect at its line, and "@" is that origin.  $TTL is not interpreted (a
// record without a TTL reads as 300).
//
// Names (Backlog #69, §28.4): every owner name is resolved to its FQDN, and
// records are compared by FQDN (case-insensitively).  With "$ORIGIN
// example.com.", the lines "www …", "www.example.com. …" and (under "$ORIGIN
// www.example.com.") "@ …" all name the same record; "www. …" names a
// different one.  A managed record whose owner is relative (or "@") with no
// $ORIGIN before it makes the zone a parse error: its FQDN depends on the
// server's configuration, which we cannot see.  (SOA/NS lines are outside the
// management boundary and are not resolved.)  On write, a record that already
// has a line keeps that line's place and its owner exactly as written (a
// relative "www" stays "www"); a new record is appended with its FQDN.
//
// Any other line that cannot be read as a record (a typo, an unknown record
// type, an unsupported directive such as $INCLUDE, a multi-line "( … )" record,
// an indented line that omits the owner name) makes the whole zone a parse
// error: Read returns Unknown and List returns an error so prune halts.  A
// record we cannot read is not proof that it is absent (§3 principle 3,
// unknown ≠ absent); silently skipping it would make the core Create a
// duplicate.  The error names the offending line number.
//
// Local ID convention: "<FQDN>/<TYPE>" (e.g. "www.example.com./A"), the FQDN
// with the case of the zone file and the type upper-cased.  List, BulkObserve
// and import report exactly this form, for a line written "www" under
// "$ORIGIN example.com." as well as for one written "www.example.com.".  The
// intent MUST use it too: ValidateIntent rejects at load an id (or a "name"
// field) that is not an FQDN ending in ".", such as "www/A", because List would
// never report it — the intent resource would look absent and the real record
// would look like an orphan.  The "name" field is the same FQDN as the id.
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
const providerVersion = "dnszone-v1.1.0"

// managedSkipTypes lists the record types that are NOT managed by this provider.
// SOA and NS are zone-metadata records that named/CoreDNS manages itself;
// writing them would break zone transfers (serial) and server configuration.
var managedSkipTypes = map[string]bool{
	"SOA": true,
	"NS":  true,
}

// knownTypes lists the record types parseLine accepts as the TYPE token.
// Generic "TYPE<n>" (RFC 3597) is accepted separately.  A token outside this
// list means the line is not a record we understand (e.g. a typo), and the
// zone becomes a parse error rather than silently losing the record.
var knownTypes = map[string]bool{
	"A": true, "AAAA": true, "AFSDB": true, "APL": true, "CAA": true,
	"CDNSKEY": true, "CDS": true, "CERT": true, "CNAME": true, "CSYNC": true,
	"DHCID": true, "DNAME": true, "DNSKEY": true, "DS": true, "EUI48": true,
	"EUI64": true, "HINFO": true, "HTTPS": true, "IPSECKEY": true, "KX": true,
	"LOC": true, "MX": true, "NAPTR": true, "NS": true, "NSEC": true,
	"NSEC3": true, "NSEC3PARAM": true, "OPENPGPKEY": true, "PTR": true,
	"RP": true, "RRSIG": true, "SMIMEA": true, "SOA": true, "SPF": true,
	"SRV": true, "SSHFP": true, "SVCB": true, "TLSA": true, "TXT": true,
	"URI": true, "ZONEMD": true,
}

// supportedDirectives lists the $-directives that are read (and preserved
// verbatim).  Others ($INCLUDE, $GENERATE, …) would hide records from us.
var supportedDirectives = map[string]bool{
	"$ORIGIN": true,
	"$TTL":    true,
}

// record is a parsed DNS resource record from the zone file.
type record struct {
	// name is the owner name resolved to its FQDN (e.g. "www.example.com."),
	// with the case it has in the file.
	name string
	// owner is the owner name as written on the line ("www", "@",
	// "www.example.com." …); kept so an Update rewrites the line in the same
	// form.
	owner string
	// ttl is the time-to-live in seconds (string, preserved as-is).
	ttl string
	// rrtype is the record type (A, AAAA, CNAME, TXT, MX, etc.).
	rrtype string
	// value is the rdata (record data).
	value string
}

// localID returns the provider-local ID for this record: "<FQDN>/<TYPE>".
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
//   - name   (string, identity, required): owner name (FQDN, same as the id's).
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
// localID format: "<FQDN>/<TYPE>" (e.g. "www.example.com./A").  A name that is
// not an FQDN is a permanent error (Unknown, never "absent"): see the package
// doc and ValidateIntent.
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

	if err := checkFQDN(name); err != nil {
		return core.Observation{
			Availability: core.AvailabilityUnknown,
			ObservedAt:   time.Now(),
		}, provider.NewPermanentError(fmt.Errorf("dnszone: Read: local id %q: %w", localID, err))
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
// The first existing line for the same {name, type} is rewritten in place with
// its owner as written; any further such lines are removed.  With no existing
// line, the new line (owner = FQDN) is appended at the end.
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
	if err := checkFQDN(name); err != nil {
		return provider.NewPermanentError(fmt.Errorf("dnszone: Apply: local id %q: %w", localID, err))
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
	// Two lines may resolve to the same id ("www" and "www.example.com.");
	// the id is listed once.
	scope := core.NewScope(nil)
	ids := make([]core.ID, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		if seen[r.localID()] {
			continue
		}
		seen[r.localID()] = true
		ids = append(ids, core.NewID(ProviderName, scope, r.localID()))
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve observes every managed record in the zone file with ONE read of
// the file (§5.7).  Each entry is exactly what Read returns for that record's
// local id, keyed like List (root scope, "<FQDN>/<TYPE>").
//
// Read finds the FIRST record whose normalised {name, type} matches, so every
// entry is built from the first such record — two lines that differ only in
// the case of the name, or in being written relative or absolute, both get
// the first line's state, as Read gives them.
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
	_ provider.Provider        = (*Provider)(nil)
	_ provider.Lister          = (*Provider)(nil)
	_ provider.BulkObserver    = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
)

// ValidateIntent rejects, at load, a desired record whose id or "name" field is
// not the FQDN form List reports (Backlog #69, §28.4).  "www/A" is rejected
// rather than resolved: the origin it is relative to lives in the zone file,
// which the loader does not read, and a guessed origin would be the very
// identity mix-up this check is for.  The "name" and "type" fields, when
// given, must name the same record as the id.
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	name, rrtype, err := splitLocalID(localID)
	if err != nil {
		return fmt.Errorf("dnszone: invalid id %q: %w", localID, err)
	}
	if err := checkFQDN(name); err != nil {
		return fmt.Errorf("dnszone: id %q: %w", localID, err)
	}
	if f, ok := fields["name"]; ok {
		if err := checkFQDN(f); err != nil {
			return fmt.Errorf("dnszone: id %q: field name %q: %w", localID, f, err)
		}
		if normaliseName(f) != normaliseName(name) {
			return fmt.Errorf("dnszone: id %q: field name %q names a different record than the id", localID, f)
		}
	}
	if f, ok := fields["type"]; ok && !strings.EqualFold(f, rrtype) {
		return fmt.Errorf("dnszone: id %q: field type %q names a different record than the id", localID, f)
	}
	return nil
}

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

// normaliseName canonicalises an FQDN for comparison: lowercased for a
// case-insensitive match.  Relative names are resolved (absName) before they
// get here; they are never compared.
func normaliseName(name string) string {
	return strings.ToLower(name)
}

// isAbsolute reports whether a zone-file name ends in an unescaped ".".
func isAbsolute(name string) bool {
	if !strings.HasSuffix(name, ".") {
		return false
	}
	bs := 0
	for i := len(name) - 2; i >= 0 && name[i] == '\\'; i-- {
		bs++
	}
	return bs%2 == 0
}

// checkFQDN returns an error unless name is an FQDN (ends in an unescaped ".").
func checkFQDN(name string) error {
	if !isAbsolute(name) {
		return fmt.Errorf("name %q is not a fully-qualified name: write the FQDN ending in \".\" (e.g. %q), not a name relative to $ORIGIN", name, name+".example.com.")
	}
	return nil
}

// absName resolves a zone-file name against origin (RFC1035 §5.1): "@" is the
// origin, a name ending in "." is already absolute, any other name is relative
// to the origin.  With no origin ("" — no $ORIGIN yet), only absolute names
// resolve.
func absName(name, origin string) (string, error) {
	if name == "@" {
		if origin == "" {
			return "", fmt.Errorf("\"@\" with no $ORIGIN before it")
		}
		return origin, nil
	}
	if isAbsolute(name) {
		return name, nil
	}
	if origin == "" {
		return "", fmt.Errorf("relative name %q with no $ORIGIN before it", name)
	}
	if origin == "." {
		return name + ".", nil
	}
	return name + "." + origin, nil
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
// rec.name is the FQDN.  The first existing line for the same {name, type} is
// rewritten in place, keeping its owner as written (so a relative "www" stays
// "www" and still resolves under the same $ORIGIN); further such lines are
// removed.  With no existing line, the record is appended with its FQDN as
// owner, which reads the same under any $ORIGIN.  Atomic write via temp file +
// rename.
func (p *Provider) writeRecord(rec record) error {
	lines, err := p.readLines()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return provider.NewTransientError(fmt.Errorf("dnszone: writeRecord: read zone: %w", err))
	}
	scanned, err := scanLines(lines)
	if err != nil {
		// Refuse to rewrite a zone we cannot fully read (unknown ≠ absent).
		return provider.NewTransientError(fmt.Errorf("dnszone: writeRecord: %s: %w", p.zonePath, err))
	}

	normName := normaliseName(rec.name)
	normType := strings.ToUpper(rec.rrtype)

	filtered := make([]string, 0, len(lines)+1)
	replaced := false
	for _, zl := range scanned {
		if !zl.ok || normaliseName(zl.rec.name) != normName || zl.rec.rrtype != normType {
			// Not this record (comment, directive, blank, other record) — preserve.
			filtered = append(filtered, zl.text)
			continue
		}
		if replaced {
			// A further line for the record we are replacing — drop it.
			continue
		}
		in := rec
		in.owner = zl.rec.owner
		newLine, err := checkedLine(in, zl.origin)
		if err != nil {
			return err
		}
		filtered = append(filtered, newLine)
		replaced = true
	}
	if !replaced {
		in := rec
		in.owner = rec.name
		newLine, err := checkedLine(in, "")
		if err != nil {
			return err
		}
		filtered = append(filtered, newLine)
	}

	return p.writeLines(filtered)
}

// checkedLine formats rec as a zone-file line and refuses (Permanent) a line
// we could not read back under origin (an unknown type, a TTL like "1h", a
// value with "(" …).  Writing it would make the whole zone a parse error on
// the next cycle and stop every record in it; refusing here confines the
// failure to this one resource.
func checkedLine(rec record, origin string) (string, error) {
	newLine := formatRecord(rec)
	if strings.ContainsAny(newLine, "\r\n") {
		// parseLine splits on any whitespace, so a line break would read back
		// as one line here yet become two lines in the file.
		return "", provider.NewPermanentError(fmt.Errorf("dnszone: writeRecord: refusing to write a record containing a line break: %q", newLine))
	}
	back, ok, perr := parseLine(newLine)
	if perr == nil && !ok {
		perr = fmt.Errorf("not a resource record: %q", newLine)
	}
	if perr == nil {
		back.name, perr = absName(back.owner, origin)
	}
	if perr == nil && (normaliseName(back.name) != normaliseName(rec.name) || back.rrtype != strings.ToUpper(rec.rrtype)) {
		perr = fmt.Errorf("reads back as %s/%s: %q", back.name, back.rrtype, newLine)
	}
	if perr != nil {
		return "", provider.NewPermanentError(fmt.Errorf("dnszone: writeRecord: refusing to write a line the zone parser cannot read back: %w", perr))
	}
	return newLine, nil
}

// deleteRecord removes all lines for {name, type} (name = FQDN) from the zone
// file, however each line writes the owner.  Atomic write via temp file +
// rename.
func (p *Provider) deleteRecord(name, rrtype string) error {
	lines, err := p.readLines()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Zone file does not exist; deletion is a no-op (idempotent).
			return nil
		}
		return provider.NewTransientError(fmt.Errorf("dnszone: deleteRecord: read zone: %w", err))
	}
	scanned, err := scanLines(lines)
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("dnszone: deleteRecord: %s: %w", p.zonePath, err))
	}

	normName := normaliseName(name)
	normType := strings.ToUpper(rrtype)

	filtered := make([]string, 0, len(lines))
	for _, zl := range scanned {
		if zl.ok && normaliseName(zl.rec.name) == normName && zl.rec.rrtype == normType {
			// Drop this line.
			continue
		}
		filtered = append(filtered, zl.text)
	}

	return p.writeLines(filtered)
}

// parseZone reads the zone file and returns all managed records, names
// resolved to FQDNs.  SOA and NS records are excluded (management boundary).
// Returns os.ErrNotExist if the zone file does not exist.
// Returns an error naming the line number if any line cannot be read
// (never a partial record set — unknown ≠ absent).
func (p *Provider) parseZone() ([]record, error) {
	lines, err := p.readLines()
	if err != nil {
		return nil, err
	}
	scanned, err := scanLines(lines)
	if err != nil {
		return nil, err
	}

	var records []record
	for _, zl := range scanned {
		if !zl.ok || managedSkipTypes[zl.rec.rrtype] {
			continue
		}
		records = append(records, zl.rec)
	}
	return records, nil
}

// zoneLine is one line of the zone file as scanLines reads it.
type zoneLine struct {
	// text is the line as it is in the file.
	text string
	// ok is true when the line is a resource record; rec is then the record.
	ok  bool
	rec record
	// origin is the $ORIGIN in effect at this line ("" before any $ORIGIN).
	origin string
}

// scanLines reads every line in order, tracking $ORIGIN, and resolves each
// managed record's owner to its FQDN.  SOA/NS owners are not resolved (their
// name stays as written): they are outside the management boundary and a zone
// may write them as "@" without a $ORIGIN, leaving the origin to the server.
// Any line that cannot be read is an error naming its line number.
func scanLines(lines []string) ([]zoneLine, error) {
	out := make([]zoneLine, 0, len(lines))
	origin := ""
	for i, line := range lines {
		zl := zoneLine{text: line, origin: origin}
		r, ok, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if !ok {
			if o, isOrigin, err := originDirective(line, origin); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			} else if isOrigin {
				origin = o
			}
			out = append(out, zl)
			continue
		}
		if managedSkipTypes[r.rrtype] {
			r.name = r.owner
		} else if r.name, err = absName(r.owner, origin); err != nil {
			return nil, fmt.Errorf("line %d: %w: %q", i+1, err, line)
		}
		zl.ok, zl.rec = true, r
		out = append(out, zl)
	}
	return out, nil
}

// originDirective reports whether line is a $ORIGIN directive and, if so,
// returns the new origin as an FQDN.  A relative argument is relative to the
// current origin (RFC1035 §5.1).
func originDirective(line, origin string) (string, bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.ToUpper(fields[0]) != "$ORIGIN" {
		return "", false, nil
	}
	if len(fields) < 2 || strings.HasPrefix(fields[1], ";") {
		return "", false, fmt.Errorf("$ORIGIN without a name: %q", line)
	}
	o, err := absName(fields[1], origin)
	if err != nil {
		return "", false, fmt.Errorf("$ORIGIN: %w: %q", err, line)
	}
	return o, true, nil
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

// parseLine parses a single zone-file line into a record whose owner is set
// as written and whose name is left empty (scanLines resolves it against
// $ORIGIN).  Returns (zero, false, nil) if the line is a comment, blank, or a supported
// directive ($ORIGIN / $TTL).  Returns a non-nil error if the line is neither
// that nor a record we can read (see the package doc).
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
func parseLine(line string) (record, bool, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ";") {
		return record{}, false, nil
	}
	if strings.HasPrefix(trimmed, "$") {
		directive := strings.ToUpper(strings.Fields(trimmed)[0])
		if !supportedDirectives[directive] {
			return record{}, false, fmt.Errorf("unsupported directive %s: %q", directive, line)
		}
		return record{}, false, nil
	}
	if trimmed != line && (line[0] == ' ' || line[0] == '\t') {
		// RFC1035 lets an indented line inherit the previous owner name; we
		// do not track that, so reading it would attach the record to a
		// wrong owner.
		return record{}, false, fmt.Errorf("owner name omitted (indented line) is not supported: %q", line)
	}
	if hasUnquotedParen(trimmed) {
		return record{}, false, fmt.Errorf("multi-line record \"( … )\" is not supported: %q", line)
	}

	fields := strings.Fields(trimmed)
	if len(fields) < 3 {
		return record{}, false, fmt.Errorf("not a resource record: %q", line)
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
		return record{}, false, fmt.Errorf("no record type: %q", line)
	}

	rrtype := strings.ToUpper(fields[idx])
	if !isKnownType(rrtype) {
		return record{}, false, fmt.Errorf("unknown record type %q: %q", fields[idx], line)
	}
	idx++

	if idx >= len(fields) {
		// No rdata — malformed.
		return record{}, false, fmt.Errorf("no record data: %q", line)
	}

	// Everything after TYPE is the rdata value.
	value := strings.Join(fields[idx:], " ")

	return record{
		owner:  name,
		ttl:    ttl,
		rrtype: rrtype,
		value:  value,
	}, true, nil
}

// isKnownType reports whether t (uppercased) is a record type parseLine
// accepts: one of knownTypes or the RFC 3597 generic form "TYPE<n>".
func isKnownType(t string) bool {
	if knownTypes[t] {
		return true
	}
	return strings.HasPrefix(t, "TYPE") && isNumeric(t[len("TYPE"):])
}

// hasUnquotedParen reports whether s contains "(" or ")" outside a quoted
// string and before a ";" comment.
func hasUnquotedParen(s string) bool {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && inQuote:
			i++ // skip the escaped character
		case c == '"':
			inQuote = !inQuote
		case c == ';' && !inQuote:
			return false
		case (c == '(' || c == ')') && !inQuote:
			return true
		}
	}
	return false
}

// formatRecord converts a record to a canonical zone-file line.
// Format: "<owner> <ttl> IN <type> <value>"
func formatRecord(r record) string {
	ttl := r.ttl
	if ttl == "" {
		ttl = "300"
	}
	return fmt.Sprintf("%s %s IN %s %s", r.owner, ttl, strings.ToUpper(r.rrtype), r.value)
}

// isNumeric returns true if s represents a non-negative integer (TTL value).
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}
