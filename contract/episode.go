package contract

// Episode admission and its artifacts.
//
// This is a SEPARATELY VERSIONED contract from the v1/v2 task and workflow
// request shapes, deliberately. Those describe a single operation with an
// outcome; an episode spans investigation, planning, implementation and
// validation, and its state must be resumable across every one of them. Folding
// that into the existing shape would mean either a v3 that breaks every
// admitted v2 request, or episode state smuggled into fields that were never
// meant to hold it.
//
// The load-bearing property of everything here is that an artifact is DATA.
// A report is a set of claims; a plan is a set of authorised steps; neither is
// an instruction to the system about what to execute. That is what lets a model
// author a plan without being able to widen its own grant.
//
// Digests are first-class. A report accepted against one source snapshot cannot
// authorise work against another, and every acceptance records the exact bytes it
// judged.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// EpisodeVersion is the admission contract version.
//
// It is distinct from Version because episode admission is not a task request
// and must never be confused with one.
const EpisodeVersion = 1

// ErrEpisodeInvalid is returned for a structurally invalid episode request.
var ErrEpisodeInvalid = errors.New("contract: the episode request is invalid")

// EpisodePhase is where an episode is.
//
// It is a PHASE, distinct from a child's supervision stage: a request may be
// WORKING during planning and working again during repair, and collapsing the
// two would make "which step are we on" unanswerable after a crash.
type EpisodePhase string

const (
	// PhaseReport means an investigation is producing a semantic report.
	PhaseReport EpisodePhase = "REPORT"
	// PhaseReportReview means a reviewer is judging that report.
	PhaseReportReview EpisodePhase = "REPORT_REVIEW"
	// PhasePlan means a planner is drafting an implementation plan.
	PhasePlan EpisodePhase = "PLAN"
	// PhasePlanAccept means local policy is deciding whether to authorise a plan.
	PhasePlanAccept EpisodePhase = "PLAN_ACCEPT"
	// PhaseImplement means an accepted plan is being executed.
	PhaseImplement EpisodePhase = "IMPLEMENT"
	// PhaseVerify means registered checks and application validation are running.
	PhaseVerify EpisodePhase = "VERIFY"
	// PhaseCompletionReview means an independent auditor judges the whole episode.
	PhaseCompletionReview EpisodePhase = "COMPLETION_REVIEW"
)

// PhaseOrder is the normal progression, used to reject a phase that moves
// backwards without an explicit return edge.
//
// Return edges exist — a plan can be rejected and the report revised — but they
// are RECORDED as revisions. Silently walking backwards would let a failing
// episode appear to be making progress.
var PhaseOrder = []EpisodePhase{
	PhaseReport, PhaseReportReview, PhasePlan, PhasePlanAccept,
	PhaseImplement, PhaseVerify, PhaseCompletionReview,
}

// Advances reports whether from precedes to in the normal progression.
func Advances(from, to EpisodePhase) bool {
	fi, ti := -1, -1
	for i, p := range PhaseOrder {
		if p == from {
			fi = i
		}
		if p == to {
			ti = i
		}
	}
	if fi < 0 || ti < 0 {
		return false
	}
	return ti > fi
}

// ConfirmedFinding is a claim an investigation makes about the code.
//
// Facts and hypotheses are SEPARATE fields rather than one "description", and
// that separation is the point: a reviewer must be able to accept the evidence
// while rejecting the conclusion drawn from it.
type ConfirmedFinding struct {
	// FindingID is stable across revisions of the report, so a plan step can
	// keep referring to the same finding after the report is reissued.
	FindingID string `json:"finding_id"`
	// RequirementID is the obligation this finding speaks to.
	RequirementID string `json:"requirement_id"`
	// Expected and Observed are kept apart so "it does this" and "it does that"
	// cannot be conflated into a single assertion.
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	// CausalRefs point at source locations supporting the observation.
	CausalRefs []string `json:"causal_refs,omitempty"`
	// EvidenceRefs point at recorded evidence items.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	// Reproduction is what an operator can run to see it.
	Reproduction string `json:"reproduction,omitempty"`
	// Confidence is DECLARED by the investigator and never treated as proof.
	Confidence string `json:"confidence,omitempty"`
	// Hypotheses are explicitly unproven explanations.
	Hypotheses []string `json:"hypotheses,omitempty"`
	// AlternativesChecked stops "we looked at one file" reading as "we looked".
	AlternativesChecked []string `json:"alternatives_checked,omitempty"`
	// Unknowns are what the investigation could NOT establish.
	//
	// It is a required field. A report that cannot say what it does not know is
	// claiming more than any investigation can support.
	Unknowns []string `json:"unknowns"`
	// ProposedVerification is how this finding could be settled.
	ProposedVerification string `json:"proposed_verification,omitempty"`
}

// SemanticReport is the investigation artifact.
//
// It is EVIDENCE. Nothing in it grants capability, and accepting it is not
// engineering PASS.
type SemanticReport struct {
	Schema    int    `json:"schema"`
	EpisodeID string `json:"episode_id"`
	Attempt   int    `json:"attempt"`
	Objective string `json:"objective"`
	// EvidenceDigest binds the report to the exact evidence it was produced from.
	// A report cannot be reused against a different evidence set.
	EvidenceDigest string `json:"evidence_digest"`
	// SourceDigest pins the checkout the investigation observed.
	SourceDigest string `json:"source_digest"`
	// ObservedProfile and ObservedModel record who produced it.
	ObservedProfile string `json:"observed_profile"`
	ObservedModel   string `json:"observed_model,omitempty"`
	// Findings are the report's substance.
	Findings []ConfirmedFinding `json:"findings"`
}

// ValidateSemanticReport enforces the report's structural rules.
//
// Everything here refuses rather than repairs. A corrected report is a report
// nobody reviewed.
func ValidateSemanticReport(r *SemanticReport) error {
	if r == nil {
		return fmt.Errorf("%w: no report supplied", ErrEpisodeInvalid)
	}
	if r.Schema != EpisodeVersion {
		return fmt.Errorf("%w: report schema %d is not supported; this build understands %d",
			ErrEpisodeInvalid, r.Schema, EpisodeVersion)
	}
	if strings.TrimSpace(r.EpisodeID) == "" {
		return fmt.Errorf("%w: the report names no episode", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(r.EvidenceDigest) == "" {
		return fmt.Errorf("%w: the report pins no evidence digest", ErrEpisodeInvalid)
	}
	if len(r.Findings) == 0 {
		return fmt.Errorf("%w: the report contains no findings", ErrEpisodeInvalid)
	}
	seen := map[string]bool{}
	for i, f := range r.Findings {
		if strings.TrimSpace(f.FindingID) == "" {
			return fmt.Errorf("%w: finding %d has no id", ErrEpisodeInvalid, i)
		}
		if seen[f.FindingID] {
			return fmt.Errorf("%w: finding id %q appears twice", ErrEpisodeInvalid, f.FindingID)
		}
		seen[f.FindingID] = true
		if strings.TrimSpace(f.Expected) == "" || strings.TrimSpace(f.Observed) == "" {
			return fmt.Errorf("%w: finding %q must state both expected and observed behaviour",
				ErrEpisodeInvalid, f.FindingID)
		}
		if len(f.EvidenceRefs) == 0 {
			// An unsupported finding is worse than an absent one: it looks like
			// evidence and is not.
			return fmt.Errorf("%w: finding %q cites no evidence", ErrEpisodeInvalid, f.FindingID)
		}
		if f.Unknowns == nil {
			return fmt.Errorf("%w: finding %q does not state what it does not know",
				ErrEpisodeInvalid, f.FindingID)
		}
	}
	return nil
}

// Digest returns the report's content digest over its canonical bytes.
func (r *SemanticReport) Digest() (string, error) { return digestJSON(r) }

// PlanStep is ONE authorised unit of implementation work.
type PlanStep struct {
	// ImportedPlanExcerpt anchors this step to exact text in a supplied plan.
	ImportedPlanExcerpt string `json:"imported_plan_excerpt,omitempty"`
	StepID              string `json:"step_id"`
	// Findings link the step to what in the report motivates it.
	Findings []string `json:"findings"`
	// RequirementIDs are the obligations the step serves.
	RequirementIDs []string `json:"requirement_ids,omitempty"`
	// DependsOn makes the ordering explicit rather than implied by listing order.
	DependsOn []string `json:"depends_on,omitempty"`
	// Repositories scope the step. A step touching a repository it did not
	// declare is a deviation, whatever it claims.
	Repositories []string `json:"repositories,omitempty"`
	// AllowedPaths is the path scope this step may write.
	//
	// It exists because repository-level scope alone is not a boundary: inside a
	// declared repository, every file looks equally authorised, so an edit the
	// plan never contemplated is indistinguishable from one it did. Without this,
	// "did the worker stay inside the plan" has no answer finer than the repo.
	AllowedPaths []string `json:"allowed_paths,omitempty"`
	// PlannedWrites names concrete files the step intends to change. Older plans
	// remain readable without it; when present these targets are checked before
	// the plan is proposed, rather than discovered after a worker turn.
	PlannedWrites []string `json:"planned_writes,omitempty"`
	// AllowedChangeKinds bounds what the step may do.
	AllowedChangeKinds []string `json:"allowed_change_kinds"`
	// ProtectedPaths may not be written by this step at all.
	ProtectedPaths []string `json:"protected_paths,omitempty"`
	// RequiredChecks must pass for the step to count as done.
	RequiredChecks []string `json:"required_checks,omitempty"`
	// AcceptanceCriteria is what the reviewer judges against.
	AcceptanceCriteria string `json:"acceptance_criteria"`
	// EvidenceObligations is what must be recorded, not merely asserted.
	EvidenceObligations []string `json:"evidence_obligations,omitempty"`
	// StopConditions end the step early rather than letting it drift.
	StopConditions []string `json:"stop_conditions,omitempty"`
	// Expected is the behaviour the step should produce.
	Expected string `json:"expected"`
}

// ImplementationPlan is the authorised work definition.
//
// It is DATA produced by a model and accepted by local policy. No step can
// enlarge the grant: argv, executables and capability are fixed by the local
// configuration, and a plan that names its own is invalid.
type ImplementationPlan struct {
	Schema    int    `json:"schema"`
	EpisodeID string `json:"episode_id"`
	Version   int    `json:"version"`
	// AcceptedReportDigest ties the plan to the report it answers.
	AcceptedReportDigest string `json:"accepted_report_digest"`
	// SourceBaseline is the checkout the plan was written against.
	SourceBaseline string `json:"source_baseline"`
	// Grant records the capability the plan was written UNDER.
	//
	// It is copied, never widened. A plan cannot grant itself more than local
	// policy allowed, and a mismatch is a refusal rather than a correction.
	Grant string `json:"grant"`
	// ExecutionProfile and ReviewProfile are named from the LOCALLY approved set.
	ExecutionProfile string `json:"execution_profile"`
	ReviewProfile    string `json:"review_profile"`
	// ImportedPlanSHA256 pins the original document when the reviewer maps an
	// existing plan into executable steps instead of drafting a replacement.
	ImportedPlanSHA256 string     `json:"imported_plan_sha256,omitempty"`
	Steps              []PlanStep `json:"steps"`
}

// ValidateImplementationPlan enforces the plan's structural rules.
func ValidateImplementationPlan(p *ImplementationPlan) error {
	if p == nil {
		return fmt.Errorf("%w: no plan supplied", ErrEpisodeInvalid)
	}
	if p.Schema != EpisodeVersion {
		return fmt.Errorf("%w: plan schema %d is not supported; this build understands %d",
			ErrEpisodeInvalid, p.Schema, EpisodeVersion)
	}
	if strings.TrimSpace(p.EpisodeID) == "" || strings.TrimSpace(p.AcceptedReportDigest) == "" {
		return fmt.Errorf("%w: the plan must name its episode and the report it answers", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(p.ExecutionProfile) == "" || strings.TrimSpace(p.ReviewProfile) == "" {
		return fmt.Errorf("%w: the plan must name its execution and review profiles explicitly", ErrEpisodeInvalid)
	}
	if p.Version < 1 {
		return fmt.Errorf("%w: plan version %d is not a version", ErrEpisodeInvalid, p.Version)
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("%w: the plan contains no steps", ErrEpisodeInvalid)
	}
	if p.ImportedPlanSHA256 != "" {
		if err := ValidateDigest(p.ImportedPlanSHA256); err != nil {
			return fmt.Errorf("%w: imported plan sha256: %v", ErrEpisodeInvalid, err)
		}
	}
	seen := map[string]bool{}
	for i, s := range p.Steps {
		if p.ImportedPlanSHA256 != "" && strings.TrimSpace(s.ImportedPlanExcerpt) == "" {
			return fmt.Errorf("%w: imported plan step %d must quote its original plan text", ErrEpisodeInvalid, i)
		}
		if strings.TrimSpace(s.StepID) == "" {
			return fmt.Errorf("%w: step %d has no id", ErrEpisodeInvalid, i)
		}
		if seen[s.StepID] {
			return fmt.Errorf("%w: step id %q appears twice", ErrEpisodeInvalid, s.StepID)
		}
		seen[s.StepID] = true
		if len(s.AllowedChangeKinds) == 0 {
			// An unbounded step is the whole grant problem in miniature.
			return fmt.Errorf("%w: step %q declares no allowed change kinds", ErrEpisodeInvalid, s.StepID)
		}
		if strings.TrimSpace(s.AcceptanceCriteria) == "" {
			return fmt.Errorf("%w: step %q has no acceptance criteria", ErrEpisodeInvalid, s.StepID)
		}
		if len(s.Findings) == 0 {
			return fmt.Errorf("%w: step %q is not linked to any report finding", ErrEpisodeInvalid, s.StepID)
		}
	}
	for _, s := range p.Steps {
		for _, dep := range s.DependsOn {
			if !seen[dep] {
				return fmt.Errorf("%w: step %q depends on %q, which is not a step in this plan",
					ErrEpisodeInvalid, s.StepID, dep)
			}
			if dep == s.StepID {
				return fmt.Errorf("%w: step %q depends on itself", ErrEpisodeInvalid, s.StepID)
			}
		}
	}
	if err := ValidatePlanWriteScope(p); err != nil {
		return err
	}
	return nil
}

// Digest returns the plan's content digest over its canonical bytes.
func (p *ImplementationPlan) Digest() (string, error) { return digestJSON(p) }

// PlanAuthorization records WHICH local policy accepted WHICH plan digest.
//
// Acceptance is the moment mutation authority changes hands, so it names the
// authority rather than merely asserting that someone approved.
type PlanAuthorization struct {
	Schema         int    `json:"schema"`
	EpisodeID      string `json:"episode_id"`
	PlanVersion    int    `json:"plan_version"`
	PlanDigest     string `json:"plan_digest"`
	ReportDigest   string `json:"report_digest"`
	SourceBaseline string `json:"source_baseline"`
	Grant          string `json:"grant"`
	// AcceptedBy names the local authority: a human operator or an approved
	// local policy. A model CANNOT appear here.
	AcceptedBy string `json:"accepted_by"`
	AcceptedAt string `json:"accepted_at"`
}

// ValidatePlanAuthorization refuses an authorisation that would hand mutation
// authority to the thing that asked for it.
func ValidatePlanAuthorization(a *PlanAuthorization, plan *ImplementationPlan) error {
	if a == nil || plan == nil {
		return fmt.Errorf("%w: the authorization or its plan is missing", ErrEpisodeInvalid)
	}
	if a.Schema != EpisodeVersion || a.EpisodeID != plan.EpisodeID || a.PlanVersion != plan.Version || a.SourceBaseline != plan.SourceBaseline {
		return fmt.Errorf("%w: authorization must match the proposed plan's schema, episode, version and source baseline", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(a.AcceptedBy) == "" {
		return fmt.Errorf("%w: the authorization names no accepting authority", ErrEpisodeInvalid)
	}
	if strings.EqualFold(strings.TrimSpace(a.AcceptedBy), plan.ExecutionProfile) {
		// The implementer approving its own plan would make the separation of
		// duties decorative from the first step.
		return fmt.Errorf("%w: %q is the execution profile and may not accept this plan", ErrEpisodeInvalid, a.AcceptedBy)
	}
	if a.PlanDigest == "" {
		return fmt.Errorf("%w: the authorization pins no plan digest", ErrEpisodeInvalid)
	}
	if a.ReportDigest != plan.AcceptedReportDigest {
		return fmt.Errorf("%w: the authorization accepts a different report than the plan answers", ErrEpisodeInvalid)
	}
	if a.Grant != plan.Grant {
		// The plan may narrow a grant, never widen it.
		return fmt.Errorf("%w: the authorization grants %q but the plan was written under %q",
			ErrEpisodeInvalid, a.Grant, plan.Grant)
	}
	return nil
}

// EpisodeRequest admits an episode and its approved template.
type EpisodeRequest struct {
	Version   int       `json:"version"`
	EpisodeID string    `json:"episode_id"`
	Origin    Origin    `json:"origin"`
	Workspace Workspace `json:"workspace"`
	// RepairBundleSHA256 selects an exact multi-repository worktree receipt
	// retained by this local store. Empty preserves legacy single-worktree
	// episodes.
	RepairBundleSHA256 string `json:"repair_bundle_sha256,omitempty"`
	Objective          string `json:"objective"`
	// Template names a LOCALLY approved episode template.
	//
	// It is a template, not a program: no shell, no argv, no condition language
	// and no mutable control configuration may arrive from a caller.
	Template       string                `json:"template"`
	ImportedPlan   *ImportedPlanDocument `json:"imported_plan,omitempty"`
	StructuredTask *StructuredTask       `json:"structured_task,omitempty"`
	// Obligations are what the episode must actually achieve.
	//
	// They are required because an episode with no obligations has nothing to be
	// judged against: every completion test would pass vacuously, and "the whole
	// required flow was verified" would mean nothing.
	Obligations  []WorkflowObligation `json:"obligations"`
	Constraints  Constraints          `json:"constraints"`
	Verification []VerificationItem   `json:"verification,omitempty"`
	Limits       Limits               `json:"limits,omitempty"`
}

// DecodeEpisodeRequest decodes and validates an episode admission request.
func DecodeEpisodeRequest(b []byte) (*EpisodeRequest, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var r EpisodeRequest
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("contract: decoding episode request: %w", err)
	}
	if r.Version != EpisodeVersion {
		return nil, fmt.Errorf("contract: episode request version %d is not supported; this build understands %d",
			r.Version, EpisodeVersion)
	}
	if strings.TrimSpace(r.EpisodeID) == "" {
		return nil, fmt.Errorf("%w: the episode has no id", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(r.Template) == "" {
		return nil, fmt.Errorf("%w: the episode names no approved template", ErrEpisodeInvalid)
	}
	if strings.TrimSpace(r.Objective) == "" {
		return nil, fmt.Errorf("%w: the episode has no objective", ErrEpisodeInvalid)
	}
	if r.RepairBundleSHA256 != "" && (len(r.RepairBundleSHA256) != 64 || strings.Trim(r.RepairBundleSHA256, "0123456789abcdef") != "") {
		return nil, fmt.Errorf("%w: repair bundle receipt must be a lowercase SHA-256 digest", ErrEpisodeInvalid)
	}
	if len(r.Obligations) == 0 {
		return nil, fmt.Errorf("%w: the episode states no obligations, and without them a completion "+
			"verdict would be vacuous", ErrEpisodeInvalid)
	}
	if err := ValidateImportedPlanDocument(r.ImportedPlan); err != nil {
		return nil, err
	}
	if r.StructuredTask != nil {
		if err := r.StructuredTask.Validate(); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// digestJSON measures an artifact's canonical bytes.
//
// Canonical means the exact encoded form: an acceptance is bound to BYTES, and
// re-encoding a struct differently between admission and judgement would let a
// plan change underneath a decision that already approved it.
func digestJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("contract: encoding an artifact for digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
