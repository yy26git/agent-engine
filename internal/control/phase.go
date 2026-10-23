package control

// Episode phase and root budgets.
//
// An episode moves through named phases: report, review it, plan it, accept the
// plan, implement it, verify it, audit it. This file is the ONLY place that
// decides whether a move is legal, for the same reason there is exactly one
// completion review: two places deciding "may we proceed" is how a control layer
// becomes advisory.
//
// Two rules carry the weight here.
//
// A phase never moves forward silently. A plan can be rejected and the report
// revised, but that is a RECORDED return edge, not a step backwards in place.
// Silently walking back would let a failing episode appear to be progressing.
//
// And a budget belongs to the EPISODE, not to a phase. A per-phase budget is
// reset by every phase change, which means a request that oscillates between
// planning and repair has an unbounded number of attempts. The budget is
// consumed from the root and survives everything the episode does.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// ErrPhaseNotAllowed is returned for a phase move the episode may not make.
var ErrPhaseNotAllowed = errors.New("agent-engine: that phase move is not allowed")

// ErrBudgetExhausted is returned when a root-wide allowance is spent.
var ErrBudgetExhausted = errors.New("agent-engine: the episode budget is exhausted")

// EpisodePhase is the control layer's view of where an episode is.
type EpisodePhase = contract.EpisodePhase

// Re-exported so callers in this package need not import contract for the
// common case.
const (
	PhaseReport           = contract.PhaseReport
	PhaseReportReview     = contract.PhaseReportReview
	PhasePlan             = contract.PhasePlan
	PhasePlanAccept       = contract.PhasePlanAccept
	PhaseImplement        = contract.PhaseImplement
	PhaseVerify           = contract.PhaseVerify
	PhaseCompletionReview = contract.PhaseCompletionReview
)

// brief shortens a digest for an operator-facing message.
func brief(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// EpisodeRoot is the durable identity and pinned-artifact record of an episode.
// Its execution and invalidation histories are append-only. The report/plan
// fields name current evidence and may be cleared when their source is
// invalidated; the old digests remain in SourceInvalidations and their artifact
// bytes remain addressable by digest.
type EpisodeRoot struct {
	ReportOnly        bool                     `json:"report_only,omitempty"`
	StructuredTask    *contract.StructuredTask `json:"structured_task,omitempty"`
	ReportTaskStarted bool                     `json:"report_task_started,omitempty"`
	ReportOutput      *StructuredReportOutput  `json:"report_output,omitempty"`
	EpisodeID         string                   `json:"episode_id"`
	Phase             string                   `json:"phase"`
	Attempt           int                      `json:"attempt"`

	// ReportProducer and ReportModel record who produced the report, read back
	// from the artifact rather than declared by the caller.
	ReportProducer string `json:"report_producer,omitempty"`
	ReportModel    string `json:"report_model,omitempty"`
	// ReportDigest pins the report currently in use. It is cleared when a
	// successor report replaces it or when source drift invalidates its evidence.
	ReportDigest string `json:"report_digest,omitempty"`
	// ImportedPlan preserves the caller's original document across restart and
	// source re-investigation. It is pinned at admission, never model-authored.
	ImportedPlan *contract.ImportedPlanDocument `json:"imported_plan,omitempty"`
	// PlanDigest and PlanVersion pin the authorised plan.
	PlanDigest   string    `json:"plan_digest,omitempty"`
	PlanVersion  int       `json:"plan_version,omitempty"`
	PlanBaseline *Snapshot `json:"plan_baseline,omitempty"`
	// ReinvestigationBaseline is the measured source epoch pinned after a
	// pre-implementation drift invalidates prior report/plan evidence. It stays
	// active until a newly accepted plan is committed against these exact bytes.
	ReinvestigationBaseline   *Snapshot                   `json:"reinvestigation_baseline,omitempty"`
	ReinvestigationGeneration int                         `json:"reinvestigation_generation,omitempty"`
	PrechargedReportRound     int                         `json:"precharged_report_round,omitempty"`
	SourceInvalidations       []EpisodeSourceInvalidation `json:"source_invalidations,omitempty"`
	// InvalidatedReason explains why an authorisation was revoked, if it was.
	InvalidatedReason string `json:"invalidated_reason,omitempty"`
	// PlanAuthorized records that local policy accepted THIS plan digest.
	//
	// It is false until an authorization naming the exact digest is committed, so
	// a plan cannot be executed on the strength of having been drafted.
	PlanAuthorized bool `json:"plan_authorized,omitempty"`
	// AuthorizedBy names the local authority that accepted it.
	AuthorizedBy string `json:"authorized_by,omitempty"`

	// StartedAt anchors the wall-clock budget.
	StartedAt string `json:"started_at"`
	// Budget is consumed from the ROOT, never reset by a phase change.
	Budget EpisodeBudget `json:"budget"`
	// PhaseHistory records every move, including returns.
	//
	// It is what makes "how did we get here" answerable after a crash, which is
	// the question a supervisor is asked first.
	PhaseHistory []EpisodePhaseMove `json:"phase_history,omitempty"`
	// Stop records a policy rejection before its terminal result is published.
	// It is committed first so a crash cannot make an episode dispatch again;
	// the controller then retries Store.PublishResult outside the state lock.
	// Optionality preserves readability of episode state written by older builds.
	Stop *EpisodeStop `json:"stop,omitempty"`

	// Execution is what the episode's phases actually DID, as distinct from
	// where they moved to.
	//
	// PhaseHistory answers "how did we get here"; it cannot answer "did the phase
	// we just entered ever run anything", because entering a phase is a decision
	// and doing the work is an effect. An episode that recorded a move without a
	// dispatch, a judgement or a verification result looks exactly like one that
	// performed them, which is the confusion this whole package exists to refuse.
	Execution EpisodeExecution `json:"execution,omitempty"`
}

// EpisodeStop is a durable, replayable request to publish BLOCKED. The marker
// carries the operator action so observers can explain a fail-stop decision
// even when publication is temporarily pending after a crash.
type EpisodeStop struct {
	Phase  string `json:"phase"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// EpisodePhaseMove is one recorded transition.
type EpisodePhaseMove struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at"`
	// Return marks a bounded return edge rather than forward progress.
	Return bool `json:"return,omitempty"`
	// Cause identifies a controller safety transition with its own audited
	// contract, rather than a template-declared task rejection route.
	Cause string `json:"cause,omitempty"`
}

// EpisodeSourceInvalidation preserves content identities when source drift
// forces a read-only re-investigation. The referenced report and plan artifacts
// remain retained by digest; these records explain why they are no longer the
// active evidence for the episode.
type EpisodeSourceInvalidation struct {
	Generation                int    `json:"generation"`
	At                        string `json:"at"`
	FromPhase                 string `json:"from_phase"`
	Reason                    string `json:"reason"`
	PreviousSourceSHA256      string `json:"previous_source_sha256,omitempty"`
	NewSourceSnapshotID       string `json:"new_source_snapshot_id"`
	NewSourceSHA256           string `json:"new_source_sha256"`
	InvalidatedReportSHA256   string `json:"invalidated_report_sha256,omitempty"`
	InvalidatedReportProducer string `json:"invalidated_report_producer,omitempty"`
	InvalidatedReportModel    string `json:"invalidated_report_model,omitempty"`
	InvalidatedPlanSHA256     string `json:"invalidated_plan_sha256,omitempty"`
	InvalidatedPlanVersion    int    `json:"invalidated_plan_version,omitempty"`
	PlanWasAuthorized         bool   `json:"plan_was_authorized,omitempty"`
	PlanAuthorizedBy          string `json:"plan_authorized_by,omitempty"`
	PreviousHandoverSHA256    string `json:"previous_handover_sha256,omitempty"`
}

const MaxEpisodeSourceInvalidations = 32

// EpisodeBudget is the root-wide allowance.
//
// Every field is a TOTAL for the episode. None is per-phase, because a per-phase
// allowance is reset by exactly the transition that a stalling request makes
// most often.
type EpisodeBudget struct {
	// ReportAttempts bounds how many reports may be produced.
	ReportAttempts int `json:"report_attempts"`
	// PlanAttempts bounds how many plans may be drafted.
	PlanAttempts int `json:"plan_attempts"`
	// RepairIterations bounds in-plan correction attempts.
	RepairIterations int `json:"repair_iterations"`
	// CompletionAudits bounds independent completion reviews.
	//
	// It is separate from RepairIterations on purpose: a request that keeps
	// failing its audit must not be able to buy more audits by repairing more.
	CompletionAudits int `json:"completion_audits"`
	// WorkerTurns bounds implementation instructions for a locally delegated
	// program card. Zero retains legacy unbounded-by-this-field behavior.
	WorkerTurns int `json:"worker_turns,omitempty"`
	// WallSeconds bounds total elapsed episode time.
	WallSeconds int `json:"wall_seconds"`
	// Consumed holds what has actually been spent.
	Consumed EpisodeBudgetConsumed `json:"consumed"`
}

// EpisodeBudgetConsumed is the spent side of the allowance.
type EpisodeBudgetConsumed struct {
	Reports         int `json:"reports"`
	Plans           int `json:"plans"`
	Repairs         int `json:"repairs"`
	CompletionAudit int `json:"completion_audits"`
	WorkerTurns     int `json:"worker_turns,omitempty"`
	WallSeconds     int `json:"wall_seconds"`
}

// DefaultEpisodeBudget is the allowance a locally approved template starts with.
func DefaultEpisodeBudget() EpisodeBudget {
	return EpisodeBudget{
		ReportAttempts:   3,
		PlanAttempts:     3,
		RepairIterations: 3,
		CompletionAudits: 2,
		WallSeconds:      3600,
	}
}

// StartEpisode installs the episode root, refusing to restart a finished one.
func (s *State) StartEpisode(episodeID string, budget EpisodeBudget, now time.Time) error {
	if strings.TrimSpace(episodeID) == "" {
		return fmt.Errorf("agent-engine: an episode needs an id")
	}
	if s.Episode != nil {
		if s.Episode.EpisodeID == episodeID {
			return nil // already started; admission is idempotent
		}
		return fmt.Errorf("agent-engine: request already carries episode %q; a second episode cannot be started inside it",
			s.Episode.EpisodeID)
	}
	if budget.ReportAttempts <= 0 || budget.PlanAttempts <= 0 || budget.CompletionAudits <= 0 {
		return fmt.Errorf("agent-engine: the episode budget must allow at least one report, plan and completion audit")
	}
	stamp := now.UTC().Format(time.RFC3339)
	s.Episode = &EpisodeRoot{
		EpisodeID: episodeID,
		Phase:     string(PhaseReport),
		Attempt:   1,
		StartedAt: stamp,
		Budget:    budget,
		PhaseHistory: []EpisodePhaseMove{
			{To: string(PhaseReport), At: stamp, Reason: "episode started"},
		},
	}
	return nil
}

// EpisodePhaseName returns the current phase, or "" when no episode is running.
func (s *State) EpisodePhaseName() string {
	if s.Episode == nil {
		return ""
	}
	return s.Episode.Phase
}

// phasePolicy is what an approved template must provide in order to gate a
// phase move.
//
// It is an interface here on purpose: the template registry is owned by a
// different file, and naming its concrete type would couple the phase machine
// to it for no gain. Declaring the ONE method the phase machine needs keeps the
// dependency explicit and narrow.
type phasePolicy interface {
	AllowsMove(from, to EpisodePhase) bool
}

// namedPhasePolicy is the richer form used when a template can identify itself
// in an error message. A template satisfies both; only the move check is
// required.
type namedPhasePolicy interface {
	phasePolicy
	Name() string
}

// policyName renders a template name when the policy can supply one.
func policyName(p phasePolicy) string {
	if n, ok := p.(namedPhasePolicy); ok && n.Name() != "" {
		return n.Name()
	}
	return "the approved template"
}

// AdvancePhase moves the episode, refusing any move the episode may not make.
//
// template may be nil for a legacy request with no template, in which case only
// forward moves in the canonical order are permitted. That is deliberately
// stricter than it sounds: without an approved template there is no declared
// return edge, so a rejected plan has nowhere recorded to go and the episode
// blocks rather than improvising one.
func (s *State) AdvancePhase(to EpisodePhase, reason string, now time.Time, template phasePolicy) error {
	if s.Episode == nil {
		return fmt.Errorf("agent-engine: no episode is running, so there is no phase to advance")
	}
	from := EpisodePhase(s.Episode.Phase)
	correction := from == to && to == PhaseImplement && template != nil && template.AllowsMove(from, to)
	if from == to && !correction {
		return nil
	}
	if template != nil && !template.AllowsMove(from, to) {
		return fmt.Errorf("%w: %s -> %s is not a declared transition of template %q",
			ErrPhaseNotAllowed, from, to, policyName(template))
	}
	if template == nil && !contract.Advances(from, to) {
		return fmt.Errorf("%w: %s -> %s is not forward progress and no template declares it",
			ErrPhaseNotAllowed, from, to)
	}
	// A phase is never entered by side effect. Implementing without an accepted
	// plan is the single failure this check exists for.
	if to == PhaseImplement && !s.Episode.PlanAuthorized {
		return fmt.Errorf("%w: implementation requires an accepted plan digest, and none is recorded",
			ErrPhaseNotAllowed)
	}
	// An in-plan retry creates a distinct dispatch round, rather than silently
	// resending the original turn. Charge here so even direct phase callers
	// cannot bypass its bound. Ordinary backwards returns are charged upstream.
	if correction {
		if err := s.ConsumeEpisodeBudget("repairs", now); err != nil {
			return err
		}
	}
	if to == PhaseCompletionReview {
		if err := s.ConsumeEpisodeBudget("completion_audits", now); err != nil {
			return err
		}
	}

	s.Episode.PhaseHistory = append(s.Episode.PhaseHistory, EpisodePhaseMove{
		From: string(from), To: string(to), Reason: reason,
		At: now.UTC().Format(time.RFC3339), Return: !contract.Advances(from, to),
	})
	if from == PhaseReport && to == PhaseReportReview && s.Episode.PrechargedReportRound == s.episodePhaseRound(PhaseReport) {
		s.Episode.PrechargedReportRound = 0
	}
	s.Episode.Phase = string(to)
	return nil
}

// AuthorizePlan records that local policy accepted THIS plan digest.
//
// The authorization names the authority rather than asserting that someone
// approved. A worker cannot appear as the accepting authority, because a
// worker that accepts its own plan makes the separation of duties decorative
// from the first step.
func (s *State) AuthorizePlan(auth contract.PlanAuthorization, plan *contract.ImplementationPlan, now time.Time) error {
	if s.Episode == nil {
		return fmt.Errorf("agent-engine: no episode is running, so no plan can be authorized")
	}
	if err := contract.ValidatePlanAuthorization(&auth, plan); err != nil {
		return err
	}
	if err := contract.ValidateImplementationPlan(plan); err != nil {
		return err
	}
	// The digest is MEASURED, not supplied. An authorization naming a digest
	// other than the plan's own would authorise bytes nobody reviewed.
	measured, err := plan.Digest()
	if err != nil {
		return err
	}
	if measured != auth.PlanDigest {
		return fmt.Errorf("%w: the authorization names plan digest %s but the plan measures %s",
			ErrPhaseNotAllowed, brief(auth.PlanDigest), brief(measured))
	}
	if auth.EpisodeID != s.Episode.EpisodeID {
		return fmt.Errorf("%w: authorization belongs to a different episode", ErrPhaseNotAllowed)
	}
	if s.Episode.PlanDigest != "" && s.Episode.PlanDigest != measured {
		return fmt.Errorf("%w: authorization must name the currently proposed plan", ErrPhaseNotAllowed)
	}
	if s.Episode.ReportDigest != "" && (auth.ReportDigest != s.Episode.ReportDigest || plan.AcceptedReportDigest != s.Episode.ReportDigest) {
		return fmt.Errorf("%w: authorization must name the current report", ErrPhaseNotAllowed)
	}
	if s.Episode.PlanAuthorized && s.Episode.PlanDigest != measured {
		// A revised plan is a NEW version requiring its own acceptance. The old
		// digest stays attributable to the effects already issued under it.
		s.Episode.Attempt++
	}
	s.Episode.PlanDigest = measured
	s.Episode.PlanVersion = auth.PlanVersion
	s.Episode.PlanAuthorized = true
	s.Episode.AuthorizedBy = auth.AcceptedBy
	s.Episode.ReportDigest = auth.ReportDigest
	return nil
}

// InvalidatePlan revokes authorisation when the source or evidence changes.
//
// A plan written against one baseline must never be applied to another checkout.
// This is called rather than left implicit because "the baseline moved" is not
// something a phase transition notices on its own.
func (s *State) InvalidatePlan(reason string) {
	if s.Episode == nil || !s.Episode.PlanAuthorized {
		return
	}
	s.Episode.PlanAuthorized = false
	s.Episode.InvalidatedReason = reason
}

// RestartForSourceDrift records the exceptional safety rollback used when the
// accepted evidence predates the measured workspace and no implementation
// step has ever been dispatched. It is deliberately separate from
// AdvancePhase: this is not a task rejection route selected by a model or
// inferred from the template; it is a controller-owned invalidation that only
// returns work to the read-only REPORT phase.
func (s *State) RestartForSourceDrift(snapshot Snapshot, previousSourceSHA256, reason, previousHandoverSHA256 string, now time.Time) (EpisodeSourceInvalidation, error) {
	var empty EpisodeSourceInvalidation
	if s == nil || s.Episode == nil {
		return empty, fmt.Errorf("agent-engine: source-drift recovery requires an admitted episode")
	}
	if s.Stage.Terminal() || s.CancellationRequested || s.Episode.Stop != nil {
		return empty, fmt.Errorf("agent-engine: stopped or terminal episodes cannot restart investigation")
	}
	if hasDispatchedImplementation(s) {
		return empty, fmt.Errorf("agent-engine: implementation has been dispatched; source drift is unattributed and cannot restart investigation")
	}
	if s.ActiveReview != nil {
		return empty, fmt.Errorf("agent-engine: an independent review is still open; source-drift re-investigation waits for read-only work to settle")
	}
	investigator, ok := s.ExecutionProfiles[RoleInvestigat]
	if !ok || !investigator.ReadOnly || !investigator.Proven {
		return empty, fmt.Errorf("agent-engine: source-drift recovery requires the episode's pinned, proven read-only investigator")
	}
	for _, step := range s.Episode.Execution.Steps {
		if (step.Phase == string(PhaseReport) || step.Phase == string(PhasePlan)) && step.Status == StepDispatched && step.ArtifactDigest == "" {
			return empty, fmt.Errorf("agent-engine: read-only %s step %q has no correlated artifact yet; source-drift re-investigation waits for it to settle", step.Phase, step.Key)
		}
	}
	if len(s.Episode.SourceInvalidations) >= MaxEpisodeSourceInvalidations {
		return empty, fmt.Errorf("agent-engine: source-drift invalidation history reached its bounded limit")
	}
	if strings.TrimSpace(snapshot.ContentDigest) == "" || strings.TrimSpace(snapshot.SnapshotID) == "" || strings.TrimSpace(snapshot.Root) == "" {
		return empty, fmt.Errorf("agent-engine: source-drift recovery requires a complete measured source snapshot")
	}
	workspace, err := canonicalPath(s.Workspace)
	if err != nil {
		return empty, err
	}
	root, err := canonicalPath(snapshot.Root)
	if err != nil || root != workspace {
		return empty, fmt.Errorf("agent-engine: re-investigation snapshot does not identify the admitted workspace")
	}
	if err := s.ConsumeEpisodeBudget("reports", now); err != nil {
		return empty, fmt.Errorf("agent-engine: source-drift re-investigation cannot start because the root report budget is exhausted: %w", err)
	}

	from := s.Episode.Phase
	stamp := now.UTC().Format(time.RFC3339Nano)
	newRound := s.episodePhaseRound(PhaseReport) + 1
	event := EpisodeSourceInvalidation{
		Generation: s.Episode.ReinvestigationGeneration + 1, At: stamp, FromPhase: from,
		Reason: reason, PreviousSourceSHA256: previousSourceSHA256,
		NewSourceSnapshotID: snapshot.SnapshotID, NewSourceSHA256: snapshot.ContentDigest,
		InvalidatedReportSHA256:   s.Episode.ReportDigest,
		InvalidatedReportProducer: s.Episode.ReportProducer,
		InvalidatedReportModel:    s.Episode.ReportModel,
		InvalidatedPlanSHA256:     s.Episode.PlanDigest, InvalidatedPlanVersion: s.Episode.PlanVersion,
		PlanWasAuthorized: s.Episode.PlanAuthorized, PlanAuthorizedBy: s.Episode.AuthorizedBy,
		PreviousHandoverSHA256: previousHandoverSHA256,
	}

	// The append-only event and old artifact references are committed before the
	// live pointers are cleared. Artifacts remain readable by their old digest.
	s.Episode.SourceInvalidations = append(s.Episode.SourceInvalidations, event)
	s.InvalidatePlan(reason)
	s.Episode.ReportDigest = ""
	s.Episode.ReportProducer = ""
	s.Episode.ReportModel = ""
	s.Episode.PlanDigest = ""
	s.Episode.PlanAuthorized = false
	s.Episode.AuthorizedBy = ""
	s.Episode.PlanBaseline = nil
	s.Episode.InvalidatedReason = reason
	s.Episode.ReinvestigationGeneration = event.Generation
	s.Episode.ReinvestigationBaseline = &snapshot
	s.Episode.PrechargedReportRound = newRound
	s.Episode.PhaseHistory = append(s.Episode.PhaseHistory, EpisodePhaseMove{
		From: from, To: string(PhaseReport), Reason: reason, At: stamp,
		Return: true, Cause: "SOURCE_DRIFT_REINVESTIGATION",
	})
	s.Episode.Phase = string(PhaseReport)
	return event, nil
}

// ConsumeEpisodeBudget charges the ROOT allowance for one unit of work.
//
// It is deliberately a single function for every budget kind, because a budget
// enforced in several places is a budget enforced in whichever place somebody
// forgets.
func (s *State) ConsumeEpisodeBudget(kind string, now time.Time) error {
	if s.Episode == nil {
		return nil
	}
	b := &s.Episode.Budget
	c := &b.Consumed

	switch kind {
	case "reports":
		if c.Reports+1 > b.ReportAttempts {
			return fmt.Errorf("%w: report attempts %d of %d are spent", ErrBudgetExhausted, c.Reports, b.ReportAttempts)
		}
		c.Reports++
	case "plans":
		if c.Plans+1 > b.PlanAttempts {
			return fmt.Errorf("%w: plan attempts %d of %d are spent", ErrBudgetExhausted, c.Plans, b.PlanAttempts)
		}
		c.Plans++
	case "repairs":
		if c.Repairs+1 > b.RepairIterations {
			return fmt.Errorf("%w: repair iterations %d of %d are spent", ErrBudgetExhausted, c.Repairs, b.RepairIterations)
		}
		c.Repairs++
	case "completion_audits":
		if c.CompletionAudit+1 > b.CompletionAudits {
			return fmt.Errorf("%w: completion audits %d of %d are spent",
				ErrBudgetExhausted, c.CompletionAudit, b.CompletionAudits)
		}
		c.CompletionAudit++
	case "worker_turns":
		if b.WorkerTurns <= 0 || c.WorkerTurns+1 > b.WorkerTurns {
			return fmt.Errorf("%w: worker turns %d of %d are spent", ErrBudgetExhausted, c.WorkerTurns, b.WorkerTurns)
		}
		c.WorkerTurns++
	default:
		return fmt.Errorf("agent-engine: unknown episode budget %q", kind)
	}

	if b.WallSeconds > 0 {
		if start, perr := time.Parse(time.RFC3339, s.Episode.StartedAt); perr == nil {
			elapsed := int(now.UTC().Sub(start).Seconds())
			c.WallSeconds = elapsed
			if elapsed > b.WallSeconds {
				return fmt.Errorf("%w: the episode has run %ds of its %ds allowance",
					ErrBudgetExhausted, elapsed, b.WallSeconds)
			}
		}
	}
	return nil
}

// EpisodeBudgetRemaining renders what is left, for operator output.
func (s *State) EpisodeBudgetRemaining() map[string]int {
	if s.Episode == nil {
		return nil
	}
	b, c := s.Episode.Budget, s.Episode.Budget.Consumed
	return map[string]int{
		"reports":           b.ReportAttempts - c.Reports,
		"plans":             b.PlanAttempts - c.Plans,
		"repairs":           b.RepairIterations - c.Repairs,
		"completion_audits": b.CompletionAudits - c.CompletionAudit,
		"wall_seconds":      b.WallSeconds - c.WallSeconds,
	}
}
