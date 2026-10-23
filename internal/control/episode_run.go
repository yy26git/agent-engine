package control

// The episode controller.
//
// This is the piece that makes a phase machine useful. `phase.go` decides
// whether a move is LEGAL; nothing yet made any move. This drives the episode
// through report → review → plan → acceptance → implementation → verification →
// independent audit, and it does so WITHOUT waiting to be told.
//
// The central design decision is that the controller never trusts the worker to
// advance the episode. A worker may report, may submit a plan, may finish a
// step — but it may not open the audit that judges it, and it may not decide
// that its own work is complete. Every transition out of a phase is driven by
// an OBSERVATION, and every judgement phase is entered by local policy rather
// than by whoever produced the artifact.
//
// A second decision: the controller is driven by the same tick the supervision
// loop already runs. There is no separate scheduler, because a second scheduler
// is a second authority over when work happens.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// EpisodeOutcome reports what one controller pass achieved.
type EpisodeOutcome struct {
	EpisodeID string
	Phase     string
	// Advanced names the phase entered, empty when nothing moved.
	Advanced string
	// Blocked explains why the episode cannot progress.
	Blocked string
	// BudgetRemaining is what is left, for operator output.
	BudgetRemaining map[string]int
}

// DriveEpisode advances one episode by at most one phase.
//
// At most one, deliberately. Each transition is a separate commit with its own
// evidence, and a controller that charged through several phases in one pass
// would make "what did we observe when we decided this" unanswerable after a
// crash.
//
// EXECUTION IS NOT OPTIONAL AND NOT AFTERWARDS. Advancing stored phase state is
// not execution: a phase that advances without having asked anyone to do anything
// produces an episode that believes it is progressing while nothing is running.
// So the pass decides at most one move and then does the work of the phase the
// episode is in AFTER that move — never two decisions, and never a phase left
// standing idle for a tick before anyone is told to work on it.
func (e *Engine) DriveEpisode(ctx context.Context, requestID string, templateName string) (EpisodeOutcome, error) {
	if handled, reason, err := e.consumeEpisodeWorkerRelease(ctx, requestID); handled {
		if err != nil {
			return EpisodeOutcome{}, err
		}
		st, serr := e.store.State(requestID)
		if serr != nil {
			return EpisodeOutcome{}, serr
		}
		out := EpisodeOutcome{Blocked: reason}
		if st.Episode != nil {
			out.EpisodeID = st.Episode.EpisodeID
			out.Phase = st.Episode.Phase
			out.BudgetRemaining = st.EpisodeBudgetRemaining()
		}
		return out, nil
	}
	out, err := e.store.DriveEpisode(requestID, templateName)
	if err != nil {
		return out, err
	}
	// A request that is not an episode is the ordinary task lifecycle's business.
	if out.EpisodeID == "" {
		return out, nil
	}
	// A delegated card may cross PLAN_ACCEPT only when the locally pinned
	// definition, fresh workspace measurements and exact plan all match. Any
	// mismatch leaves the ordinary operator exact-digest path available.
	if out.Phase == string(PhasePlanAccept) {
		if _, err := e.acceptProgramPlanIfExact(requestID); err != nil {
			return out, err
		}
	}
	blocked, xerr := e.executeEpisodePhase(ctx, requestID, templateName)
	if xerr != nil {
		return out, xerr
	}
	switch {
	case blocked == "":
	case out.Blocked == "":
		out.Blocked = blocked
	default:
		// Both facts are reported. "The phase is waiting for its report" and "no
		// role is bound to produce one" are different problems, and whoever fixes
		// one of them needs to know the other is there.
		out.Blocked = out.Blocked + "; and " + blocked
	}
	return out, nil
}

// DriveEpisode advances one admitted episode by at most one phase.
//
// It lives on the STORE, not the Engine, and that placement is deliberate:
// advancing a phase reads committed state and consults the operator's template
// registry. It performs NO transport I/O, so requiring a runtime for it would
// mean an episode could not be progressed without standing up a process whose
// identity nothing in this step depends on.
//
// The template gate is re-checked here on every pass, so a template removed from
// policy stops an episode that is already running.
func (s *Store) DriveEpisode(requestID string, templateName string) (EpisodeOutcome, error) {
	e := &Engine{store: s, now: s.now}
	return e.driveEpisode(requestID, templateName)
}

func (e *Engine) driveEpisode(requestID string, templateName string) (EpisodeOutcome, error) {
	var out EpisodeOutcome

	st, err := e.store.State(requestID)
	if err != nil {
		return out, err
	}
	if st.Episode == nil {
		return out, nil // not an episode; the ordinary task lifecycle owns it
	}
	if st.EpisodeWorkerRelease != nil {
		out.EpisodeID = st.Episode.EpisodeID
		out.Phase = st.Episode.Phase
		out.Blocked = "worker release is recorded; implementation remains paused pending local follow-up"
		return out, nil
	}
	out.EpisodeID = st.Episode.EpisodeID
	out.Phase = st.Episode.Phase

	nowFn := time.Now
	if e.now != nil {
		nowFn = e.now
	}
	now := nowFn()

	// Cancellation outranks everything. An episode being stopped must not
	// advance a single phase.
	if st.CancellationRequested {
		out.Blocked = "cancellation is pending"
		return out, nil
	}
	if st.Episode.Stop != nil {
		out.Blocked = st.Episode.Stop.Reason
		if !st.Stage.Terminal() {
			if err := e.store.publishEpisodeStop(requestID, st.Episode.Stop); err != nil {
				return out, err
			}
		}
		terminal, err := e.store.State(requestID)
		if err != nil {
			return out, err
		}
		if terminal.Episode != nil {
			out.Phase = terminal.Episode.Phase
			out.BudgetRemaining = terminal.EpisodeBudgetRemaining()
		}
		return out, nil
	}
	if st.Stage.Terminal() {
		out.Blocked = fmt.Sprintf("the request is terminal as %s", st.ResultStatus)
		return out, nil
	}
	if st.EpisodeResumeValidation != nil && st.EpisodeResumeValidation.Blocked {
		out.Blocked = "episode resume is blocked: " + st.EpisodeResumeValidation.Reason
		return out, nil
	}
	template, hasTemplate := e.templateFor(templateName)
	if templateName != "" && !hasTemplate {
		// A named template that does not exist is a REFUSAL, not a fallback to
		// the default. Falling back would run an episode under a shape nobody
		// approved.
		out.Blocked = fmt.Sprintf("template %q is not approved by this operator", templateName)
		return out, nil
	}

	var policy phasePolicy
	if hasTemplate {
		policy = template
	}

	advanced := ""
	blocked := ""

	_, uerr := e.store.UpdateCommitted(requestID, func(s *State) error {
		out.BudgetRemaining = s.EpisodeBudgetRemaining()
		if s.EpisodeWorkerRelease != nil {
			blocked = "worker release is recorded; implementation remains paused pending local follow-up"
			return nil
		}

		next, decision, derr := e.nextEpisodePhase(s, now, policy, template, hasTemplate)
		if derr != nil {
			return derr
		}
		blocked = decision
		if next == "" {
			return nil
		}
		if err := s.AdvancePhase(next, decision, now, policy); err != nil {
			// A refused policy move cannot be retried into progress. Preserve the
			// reason durably and publish BLOCKED after this transaction releases
			// the state lock.
			_, blocked, _ = episodeStop(s, now, err.Error())
			return nil
		}
		advanced = string(next)
		return nil
	})
	if uerr != nil {
		return out, uerr
	}
	out.Advanced = advanced
	out.Blocked = blocked
	adv, err := e.store.State(requestID)
	if err != nil {
		return out, err
	}
	if adv.Episode != nil {
		if adv.Episode.Stop != nil {
			out.Blocked = adv.Episode.Stop.Reason
			if !adv.Stage.Terminal() {
				if perr := e.store.publishEpisodeStop(requestID, adv.Episode.Stop); perr != nil {
					return out, perr
				}
				adv, err = e.store.State(requestID)
				if err != nil {
					return out, err
				}
			}
		}
		out.Phase = adv.Episode.Phase
		out.BudgetRemaining = adv.EpisodeBudgetRemaining()
	}
	return out, nil
}

// episodeStop commits a replayable stop marker in the same state transaction
// that observes the rejection. The terminal result is published by Store after
// that transaction releases its lock, so a crash between the two is recoverable
// and the marker prevents another phase dispatch in the meantime.
func episodeStop(s *State, now time.Time, why string) (EpisodePhase, string, error) {
	if s.Episode == nil {
		return "", why, nil
	}
	if s.Episode.Stop == nil {
		reason := fmt.Sprintf("%s. The episode is stopped as BLOCKED. Next action: the local operator must inspect the retained rejection and evidence; submit a new episode with an explicitly approved return route if work should resume.", why)
		s.Episode.Stop = &EpisodeStop{
			Phase:  s.Episode.Phase,
			Reason: reason,
			At:     now.UTC().Format(time.RFC3339),
		}
		s.StageReason = "policy stop pending BLOCKED result publication: " + reason
	}
	return "", s.Episode.Stop.Reason, nil
}

// publishEpisodeStop routes a previously committed stop marker through the
// existing immutable result publication owner. It is deliberately called only
// after the marker transaction has released the state lock.
func (s *Store) publishEpisodeStop(requestID string, stop *EpisodeStop) error {
	if stop == nil {
		return nil
	}
	_, err := s.PublishResult(requestID, contract.Result{
		Status:  contract.StatusBlocked,
		Summary: stop.Reason,
		Reason:  stop.Reason,
	})
	return err
}

// episodeMoveAllowed asks whether AdvancePhase would accept a move, without
// mutating anything.
//
// It exists for one reason: budget must never be charged for a move that is then
// refused. An episode that spent a repair on a transition its own template does
// not declare would burn through its allowance while making no progress at all —
// the precise failure a root-wide budget exists to prevent.
//
// AdvancePhase remains the authority. This asks the same question with the same
// policy object, one step earlier, and both paths use contract.Advances when no
// template is in force.
func episodeMoveAllowed(s *State, to EpisodePhase, policy phasePolicy) bool {
	if s.Episode == nil {
		return false
	}
	from := EpisodePhase(s.Episode.Phase)
	if from == to {
		return to == PhaseImplement && policy != nil && policy.AllowsMove(from, to)
	}
	if policy != nil {
		return policy.AllowsMove(from, to)
	}
	return contract.Advances(from, to)
}

// chargeEpisodeMove spends one unit of the episode's root budget for a move that
// is actually going to be made.
//
// The unit is charged BEFORE the move rather than as part of it, because a budget
// enforced in several places is a budget enforced in whichever place somebody
// forgets — and a charge that can be lost by a refused advance is not a bound.
//
// An empty kind means this move is not metered: entering plan acceptance and
// entering implementation are decisions, not attempts at work, and charging them
// against reports/plans/repairs would spend an allowance on a bookkeeping step.
func chargeEpisodeMove(s *State, to EpisodePhase, kind string, now time.Time, policy phasePolicy) error {
	if kind == "" || !episodeMoveAllowed(s, to, policy) {
		return nil
	}
	if to == PhaseReportReview && s.Episode.PrechargedReportRound == s.episodePhaseRound(PhaseReport) {
		// Source-drift recovery charged this report when it returned the episode
		// to REPORT, so completion must not charge the same attempt twice.
		return nil
	}
	return s.ConsumeEpisodeBudget(kind, now)
}

// episodeReturn takes a declared return edge, charging the episode's budget, or
// records a durable fail-stop marker whose BLOCKED result is published after
// the state transaction releases its lock.
//
// A revision is a RECORDED backwards move that costs something. There is no
// unbounded path here by construction: without a declared edge the episode
// records a stop marker and publishes BLOCKED; with one the return is charged
// against the root allowance, so repeated rejection exhausts a budget rather
// than appearing to make progress forever.
func episodeReturn(
	s *State, now time.Time, tpl EpisodeTemplate, hasTemplate bool,
	from EpisodePhase, kind, why string,
) (EpisodePhase, string, error) {
	if !hasTemplate {
		return episodeStop(s, now, fmt.Sprintf("%s, and no approved template declares a return edge from %s", why, from))
	}
	dst := ""
	for _, to := range tpl.ReturnEdges[from] {
		if dst == "" {
			dst = string(to)
		}
	}
	if dst == "" {
		return episodeStop(s, now, fmt.Sprintf("%s, and template %q declares no return edge from %s; no return route was inferred", why, tpl.Name, from))
	}
	// AdvancePhase owns the charge for an explicit IMPLEMENT re-entry.
	if EpisodePhase(dst) != from {
		if err := s.ConsumeEpisodeBudget(kind, now); err != nil {
			return episodeStop(s, now, fmt.Sprintf("%s; the bounded return route cannot be used because its root budget is exhausted: %v", why, err))
		}
	}
	if EpisodePhase(dst) == PhaseReport {
		s.InvalidatePlan("report revision requested")
		s.Episode.ReportDigest = ""
		s.Episode.PlanDigest = ""
	} else if EpisodePhase(dst) == PhasePlan {
		s.InvalidatePlan("plan revision requested")
		s.Episode.PlanDigest = ""
	}
	return EpisodePhase(dst), why, nil
}

// episodeStepAssessed reports whether a checkpoint assessment settled after the
// episode entered a phase.
//
// The implementer is never asked whether it is finished — that is the claim the
// whole system refuses to take at face value. What ends the implement phase is
// an ASSESSMENT of the step, made after the step began, by the ordinary
// checkpoint machinery this package already owns. An unreadable timestamp is
// reported as not assessed: the safe reading of "I cannot tell" is "not proven",
// never "probably fine".
func (s *State) episodeStepAssessed(p EpisodePhase) (bool, string) {
	entered := ""
	for _, m := range s.Episode.PhaseHistory {
		if m.To == string(p) {
			entered = m.At
		}
	}
	if entered == "" {
		return false, fmt.Sprintf("the episode has no recorded entry into %s", p)
	}
	if s.Checkpoint.LastReviewedAt == "" {
		return false, fmt.Sprintf("no checkpoint assessment of the %s step has settled yet", p)
	}
	reviewed, err := time.Parse(time.RFC3339, s.Checkpoint.LastReviewedAt)
	if err != nil {
		return false, "the last checkpoint assessment time is unreadable, so this step is not assessed"
	}
	entry, err := time.Parse(time.RFC3339, entered)
	if err != nil {
		return false, "the phase entry time is unreadable, so this step is not assessed"
	}
	if reviewed.Before(entry) {
		return false, fmt.Sprintf("the only checkpoint assessment on record predates this %s step", p)
	}
	return true, ""
}

// nextEpisodePhase decides the ONE phase this pass should enter, if any.
//
// It reads only committed state. It never inspects a transport and never trusts
// a worker's claim that it has finished something: the worker reports through
// artifacts, and those artifacts are what this reads.
//
// Where a phase owes WORK, this function only decides whether that work's
// recorded result permits a move. The work itself lives in episode_exec.go. The
// split matters because a decision that also performs its own side effect is a
// decision nobody can re-read: "what did we observe when we decided this" has to
// stay answerable after a crash.
func (e *Engine) nextEpisodePhase(
	s *State, now time.Time, policy phasePolicy, tpl EpisodeTemplate, hasTemplate bool,
) (EpisodePhase, string, error) {
	phase := EpisodePhase(s.Episode.Phase)
	round := s.episodePhaseRound(phase)

	switch phase {
	case PhaseReport:
		// A report exists only once it is RECORDED with its digest. Until then
		// the episode is legitimately still investigating.
		if s.Episode.ReportDigest == "" {
			return "", "awaiting an investigation report", nil
		}
		if err := chargeEpisodeMove(s, PhaseReportReview, "reports", now, policy); err != nil {
			return "", err.Error(), nil
		}
		return PhaseReportReview, "a report is ready for review", nil

	case PhaseReportReview:
		// A review opens an EFFECT; it does not advance the phase. The phase moves
		// when an answer arrives and the controller observes it.
		if s.ActiveReview != nil {
			return "", "awaiting the report reviewer's answer", nil
		}
		rec := s.episodeReviewFor(PhaseReportReview, round)
		if rec == nil {
			return "", "no report review is open", nil
		}
		if !rec.Settled {
			return "", "awaiting the report reviewer's answer", nil
		}
		switch ReviewOutcome(rec.Outcome) {
		case OutcomeContinue:
			if err := chargeEpisodeMove(s, PhasePlan, "plans", now, policy); err != nil {
				return "", err.Error(), nil
			}
			return PhasePlan, "the reviewer accepted the report", nil
		case OutcomeRedirect, OutcomeCollectEvidence:
			// The reviewer wants more before the report can be planned against.
			// That is a REVISION, so it costs a report attempt like any other.
			return episodeReturn(s, now, tpl, hasTemplate, PhaseReportReview, "reports",
				"the reviewer asked for more evidence before this report may be planned against")
		default:
			return episodeStop(s, now, fmt.Sprintf("the report review answered %s, which cannot advance the episode; planning against an unaccepted report is refused", rec.Outcome))
		}

	case PhasePlan:
		if s.Episode.PlanDigest != "" {
			if err := chargeEpisodeMove(s, PhasePlanAccept, "plans", now, policy); err != nil {
				return "", err.Error(), nil
			}
			return PhasePlanAccept, "a plan has been drafted", nil
		}
		return "", "awaiting a drafted plan", nil

	case PhasePlanAccept:
		// Acceptance is a LOCAL authority decision. There is no model to ask, so
		// this waits for one, and it waits loudly: a plan that never becomes
		// accepted must not quietly become implemented.
		if !s.Episode.PlanAuthorized {
			return "", "awaiting local plan acceptance; no model may supply it", nil
		}
		if err := chargeEpisodeMove(s, PhaseImplement, "", now, policy); err != nil {
			return "", err.Error(), nil
		}
		return PhaseImplement, "the plan is authorised", nil

	case PhaseImplement:
		if s.ActiveReview != nil {
			return "", "awaiting the implementation assessment", nil
		}
		if step, ok := s.episodeStep(episodeStepKey(PhaseImplement, round)); ok {
			if step.TurnID == "" {
				return "", "awaiting a correlated implementation turn end", nil
			}
			if s.episodeReviewFor(PhaseImplement, round) == nil {
				return "", "awaiting an independent assessment of this implementation turn", nil
			}
		}
		if review := s.episodeReviewFor(PhaseImplement, round); review != nil {
			if !review.Settled {
				return "", "awaiting the implementation assessment", nil
			}
			if ReviewOutcome(review.Outcome) != OutcomeContinue {
				return episodeReturn(s, now, tpl, hasTemplate, PhaseImplement, "repairs", "implementation assessment did not authorize verification")
			}
		}
		if s.ActiveReview == nil && s.Checkpoint.ReviewPending {
			// A checkpoint review is the controller's own assessment during
			// implementation. Its absence means the step has not been assessed,
			// which is not the same as "done".
			return "", "awaiting a checkpoint assessment of the current step", nil
		}
		// The implementer saying it finished is a CLAIM, and the claim is the
		// worker's own. What ends the step is an assessment of it, made after the
		// step began. Without one the episode waits, and waiting is correct: an
		// unassessed step is not a verified one.
		if assessed, why := s.episodeStepAssessed(PhaseImplement); !assessed {
			return "", why, nil
		}
		if err := chargeEpisodeMove(s, PhaseVerify, "", now, policy); err != nil {
			return "", err.Error(), nil
		}
		return PhaseVerify, "the implementer step has been assessed; verification is owed", nil

	case PhaseVerify:
		v := s.Episode.Execution.Verification
		if v == nil || v.Round != round {
			return "", "verification has not yet produced a result for this round", nil
		}
		if verdict := episodeVerificationVerdict(v, round); verdict != "" {
			// It ran and did not pass, or it was interrupted. Either way a revision
			// is owed, and a revision costs one repair attempt. An interrupted run is
			// deliberately routed here too: it is not a pass, and the only honest
			// response to unknown evidence is to stop and say so.
			return episodeReturn(s, now, tpl, hasTemplate, PhaseVerify, "repairs", verdict)
		}
		if s.ActiveReview == nil && s.Checkpoint.ReviewPending {
			return "", "awaiting a checkpoint assessment after verification", nil
		}
		// AdvancePhase charges the completion-audit allowance itself, because the
		// audit is spent by ENTERING the phase rather than by deciding to. Charging
		// it here as well would bill two audits for one.
		return PhaseCompletionReview, "verification ran and passed; an independent audit is owed", nil

	case PhaseCompletionReview:
		if s.ActiveReview != nil {
			return "", "awaiting the independent completion audit", nil
		}
		rec := s.episodeReviewFor(PhaseCompletionReview, round)
		if rec == nil || !rec.Settled {
			return "", "an independent completion audit is owed", nil
		}
		// An approving answer makes the request terminal inside the same decision,
		// so reaching here with a settled audit means it did not approve. The
		// episode buys a repair only while its budget says it may, and stops
		// saying it is progressing when it may not.
		return episodeReturn(s, now, tpl, hasTemplate, PhaseCompletionReview, "repairs",
			fmt.Sprintf("the independent completion audit answered %s", rec.Outcome))
	}
	return episodeStop(s, now, fmt.Sprintf("the episode is at unknown phase %q", phase))
}

// templateFor resolves an approved template by name.
func (e *Engine) templateFor(name string) (EpisodeTemplate, bool) {
	if name == "" {
		return EpisodeTemplate{}, false
	}
	return e.store.Config().EpisodeTemplates.Lookup(name)
}

// RecordReportArtifact pins a produced report against the episode.
//
// It refuses a report that does not validate, and it records the MEASURED digest
// rather than one the producer supplied — an accepted report is bound to bytes,
// not to a claim about bytes.
func (e *Engine) RecordReportArtifact(requestID string, report *contract.SemanticReport) (string, error) {
	if err := contract.ValidateSemanticReport(report); err != nil {
		return "", err
	}
	current, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if current.Episode == nil || current.Episode.EpisodeID != report.EpisodeID {
		return "", fmt.Errorf("agent-engine: report does not identify the admitted episode")
	}
	if current.EpisodeResumeValidation != nil && current.EpisodeResumeValidation.Blocked {
		return "", fmt.Errorf("agent-engine: episode resume is blocked; report intake is refused: %s", current.EpisodeResumeValidation.Reason)
	}
	if baseline := current.Episode.ReinvestigationBaseline; baseline != nil {
		if report.SourceDigest != baseline.ContentDigest {
			return "", fmt.Errorf("agent-engine: re-investigation report must cite the newly measured source baseline")
		}
		measured, measureErr := measureStableEpisodeSource(current.Workspace, e.store.Config().StoreRoot, e.now())
		if measureErr != nil {
			return "", measureErr
		}
		if measured.ContentDigest != baseline.ContentDigest {
			return "", fmt.Errorf("agent-engine: workspace drifted from the re-investigation baseline; report was not retained")
		}
	}
	digest, err := report.Digest()
	if err != nil {
		return "", err
	}
	if err := e.retainEpisodeArtifact(requestID, digest, report); err != nil {
		return "", err
	}
	_, uerr := e.store.UpdateCommitted(requestID, func(s *State) error {
		if s.Episode == nil {
			return fmt.Errorf("agent-engine: no episode is running, so no report can be recorded against one")
		}
		if s.Episode.EpisodeID != report.EpisodeID {
			return fmt.Errorf("agent-engine: the report is for episode %q but this request runs %q",
				report.EpisodeID, s.Episode.EpisodeID)
		}
		if s.EpisodeResumeValidation != nil && s.EpisodeResumeValidation.Blocked {
			return fmt.Errorf("agent-engine: episode resume became blocked before report commit")
		}
		if baseline := s.Episode.ReinvestigationBaseline; baseline != nil && report.SourceDigest != baseline.ContentDigest {
			return fmt.Errorf("agent-engine: report source does not match the active re-investigation baseline")
		}
		if s.Episode.ReportDigest != "" && s.Episode.ReportDigest != digest {
			s.InvalidatePlan("investigation report changed")
		}
		s.Episode.ReportDigest = digest
		s.Episode.ReportProducer = report.ObservedProfile
		s.Episode.ReportModel = report.ObservedModel
		return nil
	})
	return digest, uerr
}

// ProposePlan records a drafted plan WITHOUT authorising it.
//
// Drafting and accepting are separate acts by design: the planner proposes and
// local policy disposes. A method that did both would let the act of writing a
// plan hand out mutation capability.
func (e *Engine) ProposePlan(requestID string, plan *contract.ImplementationPlan) (string, error) {
	if err := contract.ValidateImplementationPlan(plan); err != nil {
		return "", err
	}
	if err := e.validateEpisodePlanPolicy(requestID, plan); err != nil {
		return "", err
	}
	current, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if current.Episode == nil || current.Episode.EpisodeID != plan.EpisodeID {
		return "", fmt.Errorf("agent-engine: plan does not identify the admitted episode")
	}
	if current.EpisodeResumeValidation != nil && current.EpisodeResumeValidation.Blocked {
		return "", fmt.Errorf("agent-engine: episode resume is blocked; plan intake is refused: %s", current.EpisodeResumeValidation.Reason)
	}
	if baseline := current.Episode.ReinvestigationBaseline; baseline != nil {
		if plan.SourceBaseline != baseline.ContentDigest {
			return "", fmt.Errorf("agent-engine: re-investigation plan must use the newly measured source baseline")
		}
		measured, measureErr := measureStableEpisodeSource(current.Workspace, e.store.Config().StoreRoot, e.now())
		if measureErr != nil {
			return "", measureErr
		}
		if measured.ContentDigest != baseline.ContentDigest {
			return "", fmt.Errorf("agent-engine: workspace drifted from the re-investigation baseline; plan was not retained")
		}
	}
	digest, err := plan.Digest()
	if err != nil {
		return "", err
	}
	if err := e.retainEpisodeArtifact(requestID, digest, plan); err != nil {
		return "", err
	}
	_, uerr := e.store.UpdateCommitted(requestID, func(s *State) error {
		if s.Episode == nil {
			return fmt.Errorf("agent-engine: no episode is running, so no plan can be proposed against one")
		}
		if s.Episode.EpisodeID != plan.EpisodeID {
			return fmt.Errorf("agent-engine: the plan is for episode %q but this request runs %q",
				plan.EpisodeID, s.Episode.EpisodeID)
		}
		if s.EpisodeResumeValidation != nil && s.EpisodeResumeValidation.Blocked {
			return fmt.Errorf("agent-engine: episode resume became blocked before plan commit")
		}
		if baseline := s.Episode.ReinvestigationBaseline; baseline != nil && plan.SourceBaseline != baseline.ContentDigest {
			return fmt.Errorf("agent-engine: plan source does not match the active re-investigation baseline")
		}
		// A plan must answer the report this episode actually has.
		if s.Episode.ReportDigest == "" || plan.AcceptedReportDigest != s.Episode.ReportDigest {
			return fmt.Errorf("%w: the plan answers report %s but the episode holds %s",
				ErrPhaseNotAllowed, brief(plan.AcceptedReportDigest), brief(s.Episode.ReportDigest))
		}
		s.Episode.PlanDigest = digest
		s.Episode.PlanVersion = plan.Version
		s.Episode.PlanAuthorized = false // proposing never authorises
		return nil
	})
	return digest, uerr
}

// EpisodeSummary renders an episode for operator output.
func (s *State) EpisodeSummary() string {
	if s.Episode == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "episode %s phase %s attempt %d", s.Episode.EpisodeID, s.Episode.Phase, s.Episode.Attempt)
	if s.Episode.PlanAuthorized {
		fmt.Fprintf(&b, " plan v%d authorised by %s", s.Episode.PlanVersion, s.Episode.AuthorizedBy)
	} else if s.Episode.PlanDigest != "" {
		b.WriteString(" plan drafted, not authorised")
	}
	if r := s.EpisodeBudgetRemaining(); r != nil {
		fmt.Fprintf(&b, " remaining reports=%d plans=%d repairs=%d audits=%d",
			r["reports"], r["plans"], r["repairs"], r["completion_audits"])
	}
	return b.String()
}
