package contract

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
)

// ValidationError is a refusal to admit a request.
//
// Admission is fail-closed. Every function here returns an error rather than a
// repaired value, because a repaired request is a request the caller did not
// write: silently filling in a missing workspace root or widening a limit to a
// default the operator never approved would make the accepted request differ
// from the submitted one while still reporting the caller's own digest.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return "contract: " + e.Reason
	}
	return fmt.Sprintf("contract: %s: %s", e.Field, e.Reason)
}

func invalid(field, format string, args ...any) *ValidationError {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Validate checks a decoded request for internal coherence and safety.
//
// It deliberately does not resolve the workspace against operator allowlists or
// the check registry; that is admission's job, because those depend on trusted
// local configuration this package cannot see. What it does enforce is
// everything that must be true regardless of configuration.
func (r *Request) Validate() error {
	if r == nil {
		return invalid("", "request is empty")
	}
	if r.Version != Version {
		return invalid("version", "unsupported schema version %d, this build accepts %d", r.Version, Version)
	}
	if err := ValidateRequestID(r.RequestID); err != nil {
		return err
	}
	if !r.Operation.Valid() {
		return invalid("operation", "unknown operation %q, expected one of INVESTIGATE, REPAIR, VERIFY, GENERATE_REPORT", r.Operation)
	}
	if strings.TrimSpace(r.Objective) == "" {
		return invalid("objective", "an objective is required; without it there is nothing to check the work against")
	}
	if err := r.validateWorkspace(); err != nil {
		return err
	}
	if err := r.validateObligations(); err != nil {
		return err
	}
	if err := r.validateEvidence(); err != nil {
		return err
	}
	if err := r.validateVerification(); err != nil {
		return err
	}
	if err := r.validateConstraints(); err != nil {
		return err
	}
	return r.validateLimits()
}

// ValidateRequestID enforces the identifier's grammar.
//
// The ID becomes a directory name under the request store, so it is validated
// rather than sanitised. Rewriting a bad ID is not an option: two distinct
// requests would be rewritten onto one directory, and then the content digest —
// the thing that exists to tell them apart — would be checking a file that one
// of them had already overwritten.
func ValidateRequestID(id string) error {
	if id == "" {
		return invalid("request_id", "required")
	}
	if len(id) > MaxRequestIDLen {
		return invalid("request_id", "is %d characters, limit is %d", len(id), MaxRequestIDLen)
	}
	if strings.HasPrefix(id, ".") {
		return invalid("request_id", "may not begin with a dot")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return invalid("request_id", "contains unsupported character %q; allowed: letters, digits, '-', '_', '.'", r)
		}
	}
	// Defence in depth: even though the grammar above excludes separators, the
	// path check makes it impossible for a future grammar change to quietly
	// reintroduce traversal.
	if id != path.Base(id) || strings.ContainsAny(id, `/\`) {
		return invalid("request_id", "must be a single path segment")
	}
	return nil
}

func (r *Request) validateWorkspace() error {
	if strings.TrimSpace(r.Workspace.Path) == "" {
		return invalid("workspace.path", "required")
	}
	// A workspace recorded but never canonicalised is the dangerous case: two
	// spellings of the same directory would take separate leases and could run
	// against each other. Refuse the obviously relative or traversing forms here
	// so they are caught at the boundary rather than after a lease is taken.
	if !strings.HasPrefix(r.Workspace.Path, "/") {
		return invalid("workspace.path", "must be absolute, got %q", r.Workspace.Path)
	}
	for _, seg := range strings.Split(r.Workspace.Path, "/") {
		if seg == ".." {
			return invalid("workspace.path", "must not contain '..'")
		}
	}
	return nil
}

func (r *Request) validateObligations() error {
	if len(r.Obligations) == 0 {
		return invalid("obligations", "at least one is required; a request with no obligations cannot be completed or failed against anything")
	}
	seen := map[string]bool{}
	for i, o := range r.Obligations {
		field := fmt.Sprintf("obligations[%d]", i)
		if strings.TrimSpace(o.ID) == "" {
			return invalid(field+".id", "required; obligations are referenced by ID and an unidentifiable one cannot be discharged")
		}
		if seen[o.ID] {
			return invalid(field+".id", "duplicate obligation ID %q", o.ID)
		}
		seen[o.ID] = true
		if strings.TrimSpace(o.Criterion) == "" {
			return invalid(field+".criterion", "required; an ID with no criterion cannot be judged")
		}
		for j, c := range o.RequiredChecks {
			if strings.TrimSpace(c) == "" {
				return invalid(fmt.Sprintf("%s.required_checks[%d]", field, j), "empty check ID")
			}
		}
	}
	return nil
}

func (r *Request) validateEvidence() error {
	seen := map[string]bool{}
	for i, e := range r.Evidence {
		field := fmt.Sprintf("evidence[%d]", i)
		if strings.TrimSpace(e.Path) == "" {
			return invalid(field+".path", "required")
		}
		if !strings.HasPrefix(e.Path, "/") {
			return invalid(field+".path", "must be absolute, got %q", e.Path)
		}
		if err := ValidateDigest(e.SHA256); err != nil {
			return invalid(field+".sha256", "%s", err)
		}
		if !e.Kind.valid() {
			return invalid(field+".kind", "unknown evidence kind %q", e.Kind)
		}
		key := e.Path + "\x00" + e.SHA256
		if seen[key] {
			return invalid(field, "duplicate evidence entry for %s", e.Path)
		}
		seen[key] = true
	}
	return nil
}

// ValidateDigest enforces the hex SHA-256 form used by every digest in the
// contract, so a malformed one is refused at the boundary instead of
// comparing unequal to everything forever.
func ValidateDigest(d string) error {
	if len(d) != 64 {
		return fmt.Errorf("must be a 64-character hex SHA-256, got %d characters", len(d))
	}
	for _, r := range strings.ToLower(d) {
		if !unicode.IsDigit(r) && !(r >= 'a' && r <= 'f') {
			return fmt.Errorf("contains non-hex character %q", r)
		}
	}
	return nil
}

func (r *Request) validateVerification() error {
	seen := map[string]bool{}
	for i, v := range r.Verification {
		field := fmt.Sprintf("verification[%d].check_id", i)
		if strings.TrimSpace(v.CheckID) == "" {
			return invalid(field, "required")
		}
		if seen[v.CheckID] {
			return invalid(field, "duplicate check %q", v.CheckID)
		}
		seen[v.CheckID] = true
	}
	return nil
}

// validateConstraints is where read-only becomes structural rather than a
// suggestion.
//
// The rule is asymmetric on purpose. REPAIR must say allow_code_changes=true
// explicitly, because a request that omits it gets no write authority and a
// repair that cannot write is not a repair. Every other operation must leave it
// false, because those operations are read-only by construction and a request
// claiming otherwise is asking for something that cannot be honoured safely.
func (r *Request) validateConstraints() error {
	if r.Operation.CodeChangeAllowed() {
		if !r.Constraints.AllowCodeChanges {
			return invalid("constraints.allow_code_changes",
				"must be true for a %s request; a repair with no write permission cannot change anything", r.Operation)
		}
	} else if r.Constraints.AllowCodeChanges {
		return invalid("constraints.allow_code_changes",
			"must be false or omitted for a %s request; that operation is read-only", r.Operation)
	}
	// A protected path that is also an allowed path is a contradiction that
	// would be resolved by whichever check ran last. Refuse it instead.
	allowed := map[string]bool{}
	for _, p := range r.Constraints.AllowedPaths {
		allowed[normalizeGlob(p)] = true
	}
	for _, p := range r.Constraints.ProtectedPaths {
		if allowed[normalizeGlob(p)] {
			return invalid("constraints.protected_paths", "%q is also listed in allowed_paths", p)
		}
	}
	return nil
}

func normalizeGlob(p string) string { return strings.TrimSpace(strings.TrimSuffix(p, "/**")) }

func (r *Request) validateLimits() error {
	for name, v := range map[string]int{
		"limits.repair_cycles":      r.Limits.RepairCycles,
		"limits.worker_generations": r.Limits.WorkerGens,
		"limits.audit_attempts":     r.Limits.AuditAttempts,
	} {
		if v < 0 {
			return invalid(name, "must not be negative, got %d", v)
		}
	}
	return nil
}

// SortedObligationIDs returns obligation IDs in sorted order, for prompts and
// deterministic output where input order carries no meaning.
func (r *Request) SortedObligationIDs() []string {
	ids := make([]string, 0, len(r.Obligations))
	for _, o := range r.Obligations {
		ids = append(ids, o.ID)
	}
	sort.Strings(ids)
	return ids
}
