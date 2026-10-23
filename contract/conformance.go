package contract

// Plan conformance receipts.
//
// An episode that reaches the end of a step still does not know whether the step
// was done. The implementer says so, the plan says what should have happened,
// and the two can agree for a long time while the diff says otherwise. The
// receipt closes that gap by recording what the SUPERVISOR measured at a safe
// boundary: the real changed-file scope, the source snapshot it was measured
// against, which checks actually ran, and everything that happened which the
// plan did not authorise.
//
// The load-bearing fields are the MEASURED ones. ChangedFiles and the check
// outcomes are observations rather than declarations, because the implementer
// does not write them and so cannot describe a diff it did not make. PlanDigest
// is measured from the plan under judgement rather than copied from a request,
// which is what stops a receipt from being attached to a plan nobody compared
// against. SourceDigest pins the checkout the observation was made against, so a
// pass earned on one tree cannot be carried forward onto another.
//
// Nothing in this file is a RESULT, and that is a deliberate structural absence
// rather than an omission. A receipt with no deviations and every check fresh is
// not PASS — it is EVIDENCE, the strongest evidence in the system, and it still
// concludes nothing. Only a correlated INDEPENDENT completion review, over
// current verification, produces PASS, and that judgement belongs to something
// that did not execute the step. There is therefore no result field and no
// accessor here that can be read as one; a caller wanting a verdict has to go
// and correlate this receipt with a review it did not produce itself.

import (
	"fmt"
	"path"
	"strings"
)

// The kinds of deviation a receipt may record.
//
// It is a CLOSED set on purpose. An unrecognised kind is refused rather than
// ignored, because a deviation nobody can name is one nobody is obliged to judge,
// and a receipt able to report a problem in a word this build does not know is a
// receipt whose silence then carries no weight at all.
var ConformanceDeviationKinds = []string{
	DeviationUndeclaredEdit,
	DeviationProtectedPathWrite,
	DeviationUnsupportedEvidence,
	DeviationRequirementChange,
	DeviationValidatorChange,
}

const (
	// DeviationUndeclaredEdit is a change the plan did not authorise.
	DeviationUndeclaredEdit = "undeclared_edit"
	// DeviationProtectedPathWrite is a write to a path the plan protected.
	DeviationProtectedPathWrite = "protected_path_write"
	// DeviationUnsupportedEvidence is a claim resting on evidence that does not
	// exist or was never produced.
	DeviationUnsupportedEvidence = "unsupported_evidence"
	// DeviationRequirementChange is work answering an obligation the plan does
	// not cover.
	DeviationRequirementChange = "requirement_change"
	// DeviationValidatorChange is a change to the thing that judges the change.
	//
	// It is listed apart from an ordinary undeclared edit because it is the one
	// that makes every other measurement suspect: editing the check so the work
	// looks finished does not fail a test, it stops the test meaning anything.
	DeviationValidatorChange = "validator_change"
)

// ConformanceReceipt is what the supervisor observed at a safe boundary.
//
// It is DATA, like everything else in this package: a record of an observation,
// addressed to a reviewer who must judge it against the plan's steps and criteria
// rather than against narrative progress. See the note above on what it is not.
type ConformanceReceipt struct {
	Schema      int    `json:"schema"`
	EpisodeID   string `json:"episode_id"`
	PlanVersion int    `json:"plan_version"`
	// PlanDigest is MEASURED from the plan under judgement, so a receipt can
	// never be attached to a plan nobody compared against.
	PlanDigest string `json:"plan_digest"`
	// StepID names the one step this receipt speaks to. A receipt covers a step,
	// not an episode: "the work is done" is unfalsifiable, "step S3 is done" is
	// checkable.
	StepID string `json:"step_id"`
	// Iteration counts the repair passes at this step, so a later receipt cannot
	// be mistaken for the one the plan was first judged against.
	Iteration int `json:"iteration"`
	// SourceDigest pins the checkout the observation was made against.
	SourceDigest string `json:"source_digest"`
	// ChangedFiles is the ACTUAL diff scope, measured — never declared.
	ChangedFiles []string `json:"changed_files"`
	// Repositories touched, for multi-repo episodes.
	Repositories []string `json:"repositories,omitempty"`
	// Checks are the verification receipts that ran.
	Checks []ConformanceCheck `json:"checks"`
	// Deviations are things that happened that the plan did not authorise.
	Deviations []ConformanceDeviation `json:"deviations,omitempty"`
	// ObservedAt records when the boundary was taken. It is provenance, not a
	// trust input: nothing here is valid because of when it was written.
	ObservedAt string `json:"observed_at"`
}

// ConformanceCheck is the outcome of one registered check against this receipt's
// source snapshot.
type ConformanceCheck struct {
	CheckID string `json:"check_id"`
	Passed  bool   `json:"passed"`
	// EvidenceRefs are the evidence backing THIS check's outcome.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	// Fresh marks the check as executed for THIS source snapshot.
	//
	// It is explicit because "an old Forge PASS cannot approve new code": a
	// passing check from a previous snapshot must not be readable as current.
	Fresh bool `json:"fresh"`
}

// ConformanceDeviation is one unauthorised thing that happened.
//
// Path is optional and empty for deviations that are not about a file; for a
// deviation measured at repository granularity it carries the repository name,
// which is the narrowest locator that measurement can honestly offer.
type ConformanceDeviation struct {
	// Kind: one of undeclared_edit, protected_path_write,
	// unsupported_evidence, requirement_change, validator_change.
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
	// Detail is required. "Something deviated" is not a reportable observation;
	// a deviation nobody can describe cannot be judged, repaired or dismissed.
	Detail       string   `json:"detail"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// Deviates reports whether anything happened that the plan did not authorise.
//
// It is a fact about the record, not a verdict about the episode. A deviation
// may be legitimate — an emergency fix the plan could not have anticipated is
// still recorded — but it is never absorbed silently, and a receipt that reports
// none has not thereby been approved.
func (r *ConformanceReceipt) Deviates() bool {
	return r != nil && len(r.Deviations) > 0
}

// HasStaleChecks reports whether any check did not run against this source
// snapshot.
//
// A single stale check means the receipt cannot support a PASS, whatever the
// others say. That is the whole reason Fresh is a field rather than an inference
// from timestamps: a passing check from a previous snapshot is not evidence about
// the current one, and a receipt that cannot tell the two apart cannot be read as
// current. The remedy is to re-run, never to restate.
func (r *ConformanceReceipt) HasStaleChecks() bool {
	if r == nil {
		return false
	}
	for _, c := range r.Checks {
		if !c.Fresh {
			return true
		}
	}
	return false
}

// Digest returns the receipt's content digest over its canonical bytes.
func (r *ConformanceReceipt) Digest() (string, error) { return digestJSON(r) }

// ValidateConformanceReceipt enforces the receipt's structural rules.
//
// Everything here refuses rather than repairs, for the same reason a malformed
// report is refused: a receipt corrected after the fact is a receipt nobody
// observed. What a valid receipt MEANS is a separate question this function does
// not answer.
func ValidateConformanceReceipt(r *ConformanceReceipt) error {
	if r == nil {
		return fmt.Errorf("%w: no conformance receipt supplied", ErrEpisodeInvalid)
	}
	if r.Schema != EpisodeVersion {
		return fmt.Errorf("%w: conformance receipt schema %d is not supported; this build understands %d",
			ErrEpisodeInvalid, r.Schema, EpisodeVersion)
	}
	if strings.TrimSpace(r.EpisodeID) == "" {
		return fmt.Errorf("%w: the conformance receipt names no episode", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(r.PlanDigest) == "" {
		return fmt.Errorf("%w: the conformance receipt pins no plan digest", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(r.StepID) == "" {
		return fmt.Errorf("%w: the conformance receipt names no step", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(r.SourceDigest) == "" {
		return fmt.Errorf("%w: the conformance receipt pins no source snapshot", ErrEpisodeInvalid)
	}
	seen := map[string]bool{}
	for i, c := range r.Checks {
		if strings.TrimSpace(c.CheckID) == "" {
			return fmt.Errorf("%w: check %d has no id", ErrEpisodeInvalid, i)
		}
		if seen[c.CheckID] {
			// Two records under one id cannot be told apart later, so neither
			// can be correlated with the review that must corroborate them.
			return fmt.Errorf("%w: check id %q appears twice", ErrEpisodeInvalid, c.CheckID)
		}
		seen[c.CheckID] = true
		if c.Passed && len(c.EvidenceRefs) == 0 {
			// A failure needs no evidence to be a fact; a pass with nothing
			// behind it is the exact thing this contract exists to catch.
			return fmt.Errorf("%w: check %q reports a pass citing no evidence", ErrEpisodeInvalid, c.CheckID)
		}
	}
	return ValidateDeviationKinds(r.Deviations)
}

// ValidateDeviationKinds refuses deviations this build cannot judge.
//
// An unknown kind is a refusal rather than a pass-through, because a receipt
// carrying one is asking a reviewer to weigh a category they were never given.
func ValidateDeviationKinds(ds []ConformanceDeviation) error {
	for i, d := range ds {
		if !isKnownDeviationKind(d.Kind) {
			return fmt.Errorf("%w: deviation %d has unknown kind %q; this build understands %v",
				ErrEpisodeInvalid, i, d.Kind, ConformanceDeviationKinds)
		}
		if strings.TrimSpace(d.Detail) == "" {
			return fmt.Errorf("%w: deviation %d (%s) explains nothing", ErrEpisodeInvalid, i, d.Kind)
		}
	}
	return nil
}

// isKnownDeviationKind answers membership of the closed set.
func isKnownDeviationKind(kind string) bool {
	for _, k := range ConformanceDeviationKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ValidateConformanceReceiptAgainstPlan additionally requires the receipt to be
// about THIS plan.
//
// It measures the plan's digest rather than comparing against a digest the
// caller supplied, so the binding cannot be asserted by the party being judged.
// A receipt naming a step the plan does not contain is refused: there is nothing
// to conform to, and an unmatched step is how unauthorised work gets reviewed as
// though it were planned.
func ValidateConformanceReceiptAgainstPlan(r *ConformanceReceipt, plan *ImplementationPlan) error {
	if err := ValidateConformanceReceipt(r); err != nil {
		return err
	}
	if plan == nil {
		return fmt.Errorf("%w: no plan supplied to judge the receipt against", ErrEpisodeInvalid)
	}
	digest, err := plan.Digest()
	if err != nil {
		return fmt.Errorf("%w: measuring the plan under judgement: %w", ErrEpisodeInvalid, err)
	}
	if r.PlanDigest != digest {
		return fmt.Errorf("%w: the receipt pins plan digest %q but the plan measures as %q",
			ErrEpisodeInvalid, r.PlanDigest, digest)
	}
	found := false
	for _, s := range plan.Steps {
		if s.StepID == r.StepID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: the receipt names step %q, which is not a step of the plan under judgement",
			ErrEpisodeInvalid, r.StepID)
	}
	return nil
}

// PlanWriteAllowed is the effective write grant shared by preflight and
// conformance: a path must be allowed by a step and protected by none.
func PlanWriteAllowed(plan *ImplementationPlan, file string) bool {
	if plan == nil {
		return false
	}
	allowed := false
	for _, step := range plan.Steps {
		for _, pattern := range step.ProtectedPaths {
			if coversPath(pattern, file) {
				return false
			}
		}
		for _, pattern := range step.AllowedPaths {
			allowed = allowed || coversPath(pattern, file)
		}
	}
	return allowed
}

// ValidatePlanWriteScope rejects concrete targets that the effective grant
// cannot write. A broad directory allowance with protected exceptions is
// legitimate; only a declared concrete target or an identical literal scope
// makes a contradiction certain before any file is changed.
func ValidatePlanWriteScope(plan *ImplementationPlan) error {
	if plan == nil {
		return fmt.Errorf("%w: no plan supplied", ErrEpisodeInvalid)
	}
	for _, step := range plan.Steps {
		for _, target := range step.PlannedWrites {
			clean := path.Clean(target)
			if target == "" || clean != target || strings.HasPrefix(target, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(target, "*?[") {
				return fmt.Errorf("%w: step %q planned write %q is not a concrete relative file path", ErrEpisodeInvalid, step.StepID, target)
			}
			inStep := false
			for _, pattern := range step.AllowedPaths {
				inStep = inStep || coversPath(pattern, target)
			}
			if !inStep || !PlanWriteAllowed(plan, target) {
				return fmt.Errorf("%w: step %q plans to write %q, but its effective allowed paths minus plan-wide protected paths do not permit it", ErrEpisodeInvalid, step.StepID, target)
			}
		}
		for _, allowed := range step.AllowedPaths {
			if strings.ContainsAny(allowed, "*?[") || strings.HasSuffix(allowed, "/**") {
				continue
			}
			for _, other := range plan.Steps {
				for _, protected := range other.ProtectedPaths {
					if allowed == protected {
						return fmt.Errorf("%w: step %q allows %q, but step %q protects that same scope", ErrEpisodeInvalid, step.StepID, allowed, other.StepID)
					}
				}
			}
		}
	}
	return nil
}

// DetectPlanDeviations returns the deviations MEASURABLE from a receipt against
// the plan it claims to answer.
//
// It measures two things, and they are not equally strong.
//
// A plan step declares REPOSITORIES and PROTECTED PATHS. It declares no allowed
// path scope, so an undeclared edit can only be detected at repository
// granularity: a changed file inside a declared repository is indistinguishable
// here from a file the plan never authorised writing. That gap is a property of
// the plan contract rather than of this file, and it is precisely why
// ChangedFiles is carried in full for a reviewer to judge — summarising a diff
// into a verdict here would hide the one thing the receipt was measured to show.
//
// Protected paths, by contrast, ARE declared, so a write under one is a real
// measurement and not an inference. It is reported even when it is harmless,
// because "this path is protected" is the plan's claim and the receipt is where
// that claim is tested.
func DetectPlanDeviations(r *ConformanceReceipt, plan *ImplementationPlan) []ConformanceDeviation {
	if r == nil || plan == nil {
		return nil
	}
	declared := map[string]bool{}
	var protected []string
	for _, s := range plan.Steps {
		for _, repo := range s.Repositories {
			if repo = strings.TrimSpace(repo); repo != "" {
				declared[repo] = true
			}
		}
		protected = append(protected, s.ProtectedPaths...)
	}

	var out []ConformanceDeviation
	for _, repo := range r.Repositories {
		repo = strings.TrimSpace(repo)
		if repo == "" || declared[repo] {
			continue
		}
		out = append(out, ConformanceDeviation{
			Kind:         DeviationUndeclaredEdit,
			Path:         repo,
			Detail:       fmt.Sprintf("repository %q was touched but no step of this plan declares it", repo),
			EvidenceRefs: []string{fmt.Sprintf("receipt:%s/%s", r.StepID, repo)},
		})
	}
	for _, f := range r.ChangedFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		for _, p := range protected {
			if coversPath(p, f) {
				out = append(out, ConformanceDeviation{
					Kind: DeviationProtectedPathWrite,
					Path: f,
					Detail: fmt.Sprintf("%q was written but the plan protects %q, which no step may write",
						f, p),
					EvidenceRefs: []string{fmt.Sprintf("receipt:%s/diff", r.StepID)},
				})
				break
			}
		}
		if !PlanWriteAllowed(plan, f) {
			isProtected := false
			for _, p := range protected {
				isProtected = isProtected || coversPath(p, f)
			}
			if !isProtected {
				out = append(out, ConformanceDeviation{
					Kind: DeviationUndeclaredEdit, Path: f,
					Detail:       "changed path is outside every accepted plan step's effective write scope",
					EvidenceRefs: []string{fmt.Sprintf("receipt:%s/diff", r.StepID)},
				})
			}
		}
	}
	return out
}

// coversPath reports whether a declared scope covers a concrete path.
//
// normalizeGlob drops a trailing "/**", which is how a scope is spelled
// everywhere else in this package, so a subtree scope means the same thing here.
// A pattern that still carries a wildcard is handed to path.Match, which does
// not cross separators — "src/*.go" must not quietly become a recursive grant.
func coversPath(pattern, p string) bool {
	g := normalizeGlob(pattern)
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	if g == "" || p == "" {
		return false
	}
	if g == p {
		return true
	}
	if strings.Contains(g, "*") {
		ok, err := path.Match(g, p)
		return err == nil && ok
	}
	return strings.HasPrefix(p, g+"/")
}
