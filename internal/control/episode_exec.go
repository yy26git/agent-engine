package control

// Episode phase EXECUTION.
//
// `phase.go` decides whether a phase move is legal and `episode_run.go` records
// it. Neither of them launches anything, which is why an admitted episode could
// sit at REPORT forever waiting for an investigation report that nothing in this
// package would ever ask anyone to produce. Stored phase advancement alone is
// not execution; this file is the part that acts.
//
// Three rules govern everything below.
//
// FIRST: a phase that needs a role dispatches to the profile bound to THAT role
// in ExecutionProfiles, and to nothing else. The binding itself is decided once,
// at admission, by episode_binding.go; this file only reads it. A role that is
// not bound produces a recorded refusal, never a substitute profile — a
// substitution would run work under an identity nobody approved, and would be
// indistinguishable, in STATE.json, from the approved one.
//
// SECOND: order. Every step is decide → commit intent → act → observe. The
// session for a role is LAUNCHED before the instruction that needs it is
// recorded, because an effect records a session id and an effect with no session
// can never be delivered or reconciled. A crash between the two leaves an idle
// session and no instruction, which the next pass finishes; the other order would
// leave an instruction addressed to a session that does not exist. Nothing here
// holds a store lock across a launch or a managed check.
//
// THIRD: honest bookkeeping. A dispatch that could not happen, a verification
// attempt that was interrupted, a review whose verdict cannot be found — each is
// RECORDED as what it is. None is ever relabelled as progress, and none is
// retried silently, because a retry that re-runs an effect whose outcome is
// unknown is how an agent is told to do the same thing twice.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// Episode execution statuses.
const (
	// StepDispatched means an instruction for this step is durably recorded.
	StepDispatched = "DISPATCHED"
	// StepRefused means the step could not be attempted at all, and why.
	//
	// Only a PERMANENT refusal is recorded this way — an unbound role, which
	// cannot change without a new admission. A transient failure (a transport
	// that would not launch) is deliberately not recorded, so the next pass tries
	// again instead of inheriting a block it never earned.
	StepRefused = "REFUSED"
)

// MaxEpisodeSteps bounds the durable dispatch ledger.
//
// It is a backstop, not a garbage collector: the episode budget caps reports,
// plans, repairs and audits, so a well-behaved episode records an order of
// magnitude fewer steps than this.
const MaxEpisodeSteps = 64

// EpisodeStep is one phase's attempt to have its work done.
type EpisodeStep struct {
	// Key identifies the step: phase and round. It is the idempotency key — a
	// second pass over the same step finds this record and does nothing.
	Key   string `json:"key"`
	Phase string `json:"phase"`
	Round int    `json:"round"`
	// Role is the profile role this step dispatches to, empty when the phase
	// needs no model at all.
	Role string `json:"role,omitempty"`
	// Profile is the PINNED profile identity that role resolved to at admission.
	Profile string `json:"profile,omitempty"`
	// Session is the confirmed session the instruction is addressed to.
	Session string `json:"session,omitempty"`
	// EffectID is the durable instruction, so a blocked delivery is visible as
	// blocked rather than as a step that quietly did nothing.
	EffectID string `json:"effect_id,omitempty"`
	Status   string `json:"status"`
	// Reason explains a refusal, or names what the step was for.
	Reason                     string                    `json:"reason,omitempty"`
	At                         string                    `json:"at"`
	ArtifactNonce              string                    `json:"artifact_nonce,omitempty"`
	ArtifactDigest             string                    `json:"artifact_digest,omitempty"`
	ArtifactEvidenceDigest     string                    `json:"artifact_evidence_digest,omitempty"`
	ArtifactCorrectionEffectID string                    `json:"artifact_correction_effect_id,omitempty"`
	ArtifactCorrectionTurn     string                    `json:"artifact_correction_turn,omitempty"`
	EvidenceRefusal            *EpisodeEvidenceRejection `json:"evidence_refusal,omitempty"`
	TurnID                     string                    `json:"turn_id,omitempty"`
	SourceModel                string                    `json:"source_model,omitempty"`
	ModelEcho                  string                    `json:"model_echo,omitempty"`
	Generation                 int                       `json:"generation"`
}

// EpisodeReview is the episode's own record of one judgement it opened.
//
// It exists because the shared review machinery closes a review by clearing the
// active slot: after a verdict lands, the state no longer says WHICH verdict it
// was. An episode cannot advance on "something was reviewed" — that is exactly
// the silent pass this system refuses — so the settlement is recorded where the
// episode can read it, by the one function that applies an answer.
type EpisodeReview struct {
	Phase    string `json:"phase"`
	Round    int    `json:"round"`
	Kind     string `json:"kind"`
	ReviewID string `json:"review_id"`
	// ReviewerSession is the independent session the judgement was asked of.
	ReviewerSession string `json:"reviewer_session"`
	OpenedAt        string `json:"opened_at"`
	// Settled and Outcome are written when the answer is applied. A PASS is not
	// recorded here because a PASS makes the request terminal inside the same
	// decision, which is a fact the state already carries.
	Settled   bool   `json:"settled,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	SettledAt string `json:"settled_at,omitempty"`
	ModelEcho string `json:"model_echo,omitempty"`
	// ReportBody is the correlated review's feedback, not execution authority.
	ReportBody string `json:"report_body,omitempty"`
}

// EpisodeVerification is the VERIFY phase's own record of the run it performed.
//
// A verification result is not the same thing as a phase move, and conflating
// them is how "the checks passed" becomes "the episode finished". The record is
// committed BEFORE the checks run and completed afterwards, so a run that was
// interrupted is visible as an interrupted run rather than silently repeated.
type EpisodeVerification struct {
	Round    int            `json:"round"`
	Required []string       `json:"required,omitempty"`
	Receipts []CheckReceipt `json:"receipts,omitempty"`
	// ContentSHA is the digest of the content the receipts were taken against.
	ContentSHA string `json:"content_sha256,omitempty"`
	// Complete is false between the intent and the observation. A false value at
	// read time means the run was interrupted and its outcome is unknown.
	Complete  bool   `json:"complete"`
	Satisfied bool   `json:"satisfied,omitempty"`
	Reason    string `json:"reason,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// EpisodeExecution is the episode's durable record of what its phases DID.
type EpisodeExecution struct {
	TurnReceipts []EpisodeTurnReceipt          `json:"turn_receipts,omitempty"`
	Steps        []EpisodeStep                 `json:"steps,omitempty"`
	Reviews      []EpisodeReview               `json:"reviews,omitempty"`
	Verification *EpisodeVerification          `json:"verification,omitempty"`
	Conformance  []contract.ConformanceReceipt `json:"conformance,omitempty"`
}

// EpisodeWorkerRelease is a local operator request to release the currently
// bound implementation session. It intentionally carries no candidate profile
// or replacement session: release proof and replacement authorization are
// separate decisions.
type EpisodeWorkerRelease struct {
	Status          string `json:"status"`
	RequestedAt     string `json:"requested_at"`
	BindingSHA256   string `json:"binding_sha256"`
	EpisodeID       string `json:"episode_id"`
	Phase           string `json:"phase"`
	Generation      int    `json:"generation"`
	Backend         string `json:"backend"`
	SessionID       string `json:"session_id"`
	Profile         string `json:"profile"`
	ProfileSHA256   string `json:"profile_sha256"`
	PlanSHA256      string `json:"plan_sha256"`
	SourceSHA256    string `json:"source_sha256"`
	ProgramSHA256   string `json:"program_sha256,omitempty"`
	CardSHA256      string `json:"card_sha256,omitempty"`
	ProcessPID      int    `json:"process_pid,omitempty"`
	ProcessStart    string `json:"process_start,omitempty"`
	CompletedAt     string `json:"completed_at,omitempty"`
	Reason          string `json:"reason,omitempty"`
	QuiescenceProof string `json:"quiescence_proof,omitempty"`
}

const (
	WorkerReleaseRequested = "requested"
	WorkerReleaseBlocked   = "blocked"
	WorkerReleaseReleased  = "released"
)

// episodeRoleForPhase names the role each phase dispatches to.
//
// It is one table rather than a decision inside each dispatcher because "which
// role fills this phase" is a single question with a single answer, and answering
// it in seven places is how two of the seven drift.
//
// PLAN_ACCEPT and VERIFY are deliberately ABSENT rather than mapped to empty: plan
// acceptance is a local authority decision and verification is a managed command.
// Neither is a model's to perform, and naming a role for them would be an
// invitation.
var episodeRoleForPhase = map[EpisodePhase]ProfileRole{
	PhaseReport:           RoleInvestigat,
	PhaseReportReview:     RoleReviewer,
	PhasePlan:             RoleReviewer,
	PhaseImplement:        RoleWorker,
	PhaseCompletionReview: RoleReviewer,
}

// episodePhaseRound counts how many times the episode has ENTERED a phase.
//
// It is derived from PhaseHistory rather than kept as its own counter, so a round
// number cannot disagree with the moves that produced it. StartEpisode seeds the
// first entry, so the phase an episode begins in is round 1.
func (s *State) episodePhaseRound(p EpisodePhase) int {
	if s.Episode == nil {
		return 0
	}
	n := 0
	for _, m := range s.Episode.PhaseHistory {
		if m.To == string(p) {
			n++
		}
	}
	return n
}

// episodeStepKey is the identity of one attempt at one phase.
func episodeStepKey(phase EpisodePhase, round int) string {
	return fmt.Sprintf("%s#%d", phase, round)
}

// episodeStep returns a recorded step.
func (s *State) episodeStep(key string) (EpisodeStep, bool) {
	if s.Episode == nil {
		return EpisodeStep{}, false
	}
	for _, st := range s.Episode.Execution.Steps {
		if st.Key == key {
			return st, true
		}
	}
	return EpisodeStep{}, false
}

// recordEpisodeStep appends one attempt to the ledger.
func (s *State) recordEpisodeStep(step EpisodeStep) {
	if s.Episode == nil {
		return
	}
	s.Episode.Execution.Steps = append(s.Episode.Execution.Steps, step)
	if n := len(s.Episode.Execution.Steps); n > MaxEpisodeSteps {
		s.Episode.Execution.Steps = s.Episode.Execution.Steps[n-MaxEpisodeSteps:]
	}
}

func (s *State) replaceEpisodeStep(step EpisodeStep) {
	for i := range s.Episode.Execution.Steps {
		if s.Episode.Execution.Steps[i].Key == step.Key {
			s.Episode.Execution.Steps[i] = step
			return
		}
	}
}

// episodeReviewFor returns the judgement this episode opened for a phase round.
func (s *State) episodeReviewFor(phase EpisodePhase, round int) *EpisodeReview {
	if s.Episode == nil {
		return nil
	}
	for i := range s.Episode.Execution.Reviews {
		if s.Episode.Execution.Reviews[i].Phase == string(phase) &&
			s.Episode.Execution.Reviews[i].Round == round {
			return &s.Episode.Execution.Reviews[i]
		}
	}
	return nil
}

// recordEpisodeReviewSettlement notes how a judgement this episode opened ended.
//
// It is called from the one place a review answer is applied, and it matches on
// the review's own identity: a review the episode did not open — a supervision
// checkpoint, a task completion assessment — is none of this episode's business
// and is left alone.
//
// Only a NON-APPROVING settlement is recorded. An approving completion answer
// makes the request terminal inside the same decision, so "the audit passed" is
// already a durable fact, and recording it twice would create a second statement
// that could later disagree with the first.
func (s *State) recordEpisodeReviewSettlement(active *ActiveReview, ans ReviewAnswer, at string) {
	if s.Episode == nil || active == nil {
		return
	}
	for i := range s.Episode.Execution.Reviews {
		rec := &s.Episode.Execution.Reviews[i]
		if rec.ReviewID != active.ReviewID || rec.Settled {
			continue
		}
		rec.Settled = true
		rec.Outcome = string(ans.Outcome)
		rec.SettledAt = at
		rec.ModelEcho = ans.ModelEcho
		rec.ReportBody = ans.Body
		return
	}
}

// nowFn is the engine's clock, defaulted.
func (e *Engine) nowFn() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e *Engine) stamp() string { return e.nowFn().UTC().Format(time.RFC3339) }

// --- execution -------------------------------------------------------------

// executeEpisodePhase performs the work the current phase owes.
//
// It returns a blocking reason, never a success: the caller decides whether the
// phase may move, and a phase whose work did not happen must not move on the
// strength of having been asked for.
func (e *Engine) executeEpisodePhase(ctx context.Context, requestID, templateName string) (string, error) {
	if e.opts.DryRun {
		// A dry run replaces the transport with a printer and must not launch a
		// session or run a managed command, so it performs no phase execution at
		// all. That is a refusal, not a simulated success.
		return "", nil
	}
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if st.Episode == nil || st.CancellationRequested || st.RuntimeBlock != nil || st.Stage.Terminal() {
		return "", nil
	}
	if err := e.consumeEpisodeReceipts(requestID, st); err != nil {
		return err.Error(), nil
	}
	st, err = e.store.State(requestID)
	if err != nil {
		return "", err
	}
	tpl, hasTemplate := e.templateFor(templateName)
	phase := EpisodePhase(st.Episode.Phase)
	round := st.episodePhaseRound(phase)

	switch phase {
	case PhaseVerify:
		return e.executeEpisodeVerification(requestID, tpl, hasTemplate, round)
	case PhaseReportReview:
		return e.openEpisodeJudgement(ctx, requestID, PhaseReportReview, round, ReviewCheckpoint, reportReviewPrompt)
	case PhaseCompletionReview:
		return e.openEpisodeJudgement(ctx, requestID, PhaseCompletionReview, round, ReviewCompletion, completionReviewPrompt)
	}

	role := episodeRoleForPhase[phase]
	if role == "" {
		return "", nil
	}
	objective, _ := e.episodeObjective(requestID)
	return e.dispatchEpisodeRole(ctx, requestID, phase, round, role, objective)
}

// driveEpisodeIfPresent runs the episode tick for a request that carries an
// episode, and does nothing at all for one that does not.
//
// A request carries EITHER a task lifecycle OR an episode, never both, so this
// is a shape test rather than a fallback: a task request never acquires episode
// execution, and an episode never runs the task lifecycle. Guarding on the
// recorded shape is what keeps the two from growing into a second, overlapping
// authority over "what does this request do next".
//
// It sits beside DriveTaskLifecycle on the supervision tick, not inside it, so
// neither path can change the other's behaviour.
//
// Errors propagate. A blocked phase is NOT an error — executeEpisodePhase
// returns a reason string for that — but a genuine failure to dispatch must not
// be swallowed, because a supervisor that reports success while its episode
// stopped moving is the exact failure this layer exists to prevent.
func (e *Engine) driveEpisodeIfPresent(ctx context.Context, requestID string) error {
	st, err := e.store.State(requestID)
	if err != nil {
		return err
	}
	if st.Episode == nil || e.opts.DryRun {
		return nil
	}
	return e.reconcileOnExit(ctx, requestID, func() error {
		// Use the controller, not just its executor: committed artifacts and
		// settled reviews must advance the phase without an external drive call.
		out, err := e.DriveEpisode(ctx, requestID, st.EpisodeTemplate)
		if err != nil {
			return fmt.Errorf("agent-engine: driving episode phase %s: %w", st.Episode.Phase, err)
		}
		block := ""
		if out.Blocked != "" {
			block = fmt.Sprintf("episode %s waiting at %s: %s", requestID, out.Phase, out.Blocked)
		}
		if block != e.lastEpisodeBlock {
			e.lastEpisodeBlock = block
			if block != "" {
				e.logf("%s", block)
			}
		}
		return nil
	})
}

// episodeObjective reads the admitted objective for operator-facing text.
//
// A missing objective is not an error here: it is display context inside an
// instruction, and a prompt missing its first line is a worse prompt, not an
// unsound decision.
func (e *Engine) episodeObjective(requestID string) (string, error) {
	adm, err := e.store.loadRequestDir(requestID)
	if err != nil {
		return "", err
	}
	return adm.Request.Objective, nil
}

// dispatchEpisodeRole tells one role what this phase needs, exactly once.
//
// Once means once per phase round, permanently. The step ledger is the record,
// and the durable instruction it points at is the second line of defence: an
// effect that cannot be proven delivered stays BLOCKED and is never silently
// re-sent, because a second send tells an agent to do the same work twice.
func (e *Engine) dispatchEpisodeRole(
	ctx context.Context, requestID string, phase EpisodePhase, round int,
	role ProfileRole, objective string,
) (string, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if role == RoleWorker && st.EpisodeWorkerRelease != nil {
		return "worker release is recorded; no implementation instruction may be dispatched", nil
	}
	key := episodeStepKey(phase, round)
	if role == RoleWorker {
		if err := st.VerifyRepairBranch(); err != nil {
			return err.Error(), nil
		}
	}
	if step, done := st.episodeStep(key); done {
		if phase == PhaseImplement && step.TurnID != "" {
			return e.openEpisodeJudgement(ctx, requestID, phase, round, ReviewCheckpoint, func(s *State) string {
				return "Assess the implementation against the accepted plan and measured conformance receipts. CONTINUE permits managed verification; it does not approve completion.\n"
			})
		}
		if step.Status == StepDispatched && (phase == PhaseReport || phase == PhasePlan) && step.ArtifactDigest == "" {
			reason, err := e.reconcileEpisodeProducer(ctx, requestID, st, step)
			if err != nil {
				return "", err
			}
			if reason != "" {
				return reason, nil
			}
		}
		return e.stepOutcome(st, step), nil
	}

	// The binding is the admission-time decision. An unbound role is refused and
	// recorded as a permanent refusal, because the only thing that can change it
	// is a new admission under a template that binds it.
	ref, bound := st.ExecutionProfiles[role]
	if !bound {
		reason := fmt.Sprintf(
			"the %s role is not bound in this episode, so phase %s has nobody to run it; the episode is "+
				"still awaiting the work that phase produces", role, phase)
		if _, err := e.store.UpdateCommitted(requestID, func(s *State) error {
			s.recordEpisodeStep(EpisodeStep{
				Key: key, Phase: string(phase), Round: round,
				Status: StepRefused, Reason: reason, At: e.stamp(),
			})
			return nil
		}); err != nil {
			return "", err
		}
		return reason, nil
	}
	if (phase == PhaseReport || phase == PhasePlan) && st.Episode.ReinvestigationBaseline != nil {
		measured, measureErr := measureStableEpisodeSource(st.Workspace, e.store.Config().StoreRoot, e.now())
		if measureErr != nil {
			return "re-investigation refused because the current source cannot be measured stably: " + measureErr.Error(), nil
		}
		if measured.ContentDigest != st.Episode.ReinvestigationBaseline.ContentDigest {
			return "re-investigation paused because the workspace changed again after its source baseline was pinned; resume validation must record a new baseline before REPORT can dispatch", nil
		}
	}

	// Pin content before the first worker can change it. Episode phases do not
	// enter StageWorking, so the task lifecycle's baseline hook cannot do this.
	// RecordBaseline preserves the first measurement across retries and repairs.
	if role == RoleWorker {
		if err := e.store.ensureBaseline(requestID); err != nil {
			return "", fmt.Errorf("agent-engine: cannot dispatch worker without a baseline: %w", err)
		}
	}

	// The session is launched BEFORE the instruction is recorded: an effect names
	// a session, and an effect addressed to a session that does not exist can
	// neither be delivered nor reconciled. A crash in this window leaves an idle
	// session and no instruction, which the next pass completes.
	session, err := e.ensureEpisodeRoleSession(ctx, requestID, role)
	if err != nil {
		// Deliberately NOT recorded. A launch that failed for a reason that may
		// pass is not a permanent refusal, and writing one would convert a
		// transient outage into a block the episode can never leave.
		return fmt.Sprintf("the %s role could not be given a session, so phase %s was not started: %v",
			role, phase, err), nil
	}

	artifactContext, _, err := e.episodeArtifactContextWithBudget(requestID, st)
	if err != nil {
		return "", err
	}
	nonce, err := randomToken()
	if err != nil {
		return "", err
	}
	message := episodeStepPrompt(st, phase, key, objective) + artifactContext
	source, err := TakeSnapshot(st.Workspace, e.nowFn(), e.store.Config().StoreRoot)
	if err != nil {
		return "", err
	}
	message += "\nMeasured source baseline: " + source.ContentDigest + "\n"
	if phase == PhaseReport || phase == PhasePlan {
		schema, err := e.episodeArtifactSchema(requestID, st, phase)
		if err != nil {
			return "", err
		}
		message += schema
		message += episodeArtifactInstruction(requestID, key, nonce, st.Generation)
	}
	stamp := e.stamp()
	if _, err := e.store.UpdateCommitted(requestID, func(s *State) error {
		if role == RoleWorker && s.EpisodeWorkerRelease != nil {
			return fmt.Errorf("worker release is recorded; implementation dispatch is paused")
		}
		// Re-checked under the lock: two writers must not both believe they own
		// the only instruction for this step.
		if _, done := s.episodeStep(key); done {
			return nil
		}
		// A reviewer sharing the implementer's lineage is refused here, at the
		// moment the instruction is created, rather than when an answer arrives
		// and turns out to be self-certifying.
		if role != RoleWorker && s.IsImplementationSession(session) {
			return fmt.Errorf("%w: session %s is inside this episode's implementation lineage and cannot fill %s",
				ErrProfileNotIsolated, session, role)
		}
		if role == RoleWorker && s.ProgramBinding != nil {
			if err := s.ConsumeEpisodeBudget("worker_turns", e.now()); err != nil {
				return err
			}
		}
		// Derived inside the transaction, from the revision this decision is
		// actually taken at.
		effectID := EffectID(s.Revision, session, string(role), message)
		if err := s.RecordEffect(Effect{
			EffectID:  effectID,
			SessionID: session,
			Role:      string(role),
			Message:   message,
			Reason:    key,
			CreatedAt: stamp,
		}); err != nil {
			return err
		}
		s.recordEpisodeStep(EpisodeStep{
			Key: key, Phase: string(phase), Round: round, Role: string(role),
			Profile: ref.Name, Session: session, EffectID: effectID,
			ArtifactNonce: nonce, Generation: s.Generation,
			Status: StepDispatched, Reason: "phase " + string(phase) + " is owed to the " + string(role) + " role",
			At: stamp,
		})
		return nil
	}); err != nil {
		return "", err
	}

	// Delivered while the follow is still live. A dry run never reaches here.
	if _, err := e.dispatch.Drain(ctx, requestID); err != nil {
		return fmt.Sprintf("the %s instruction for phase %s could not be dispatched: %v", role, phase, err), nil
	}
	if fresh, ferr := e.store.State(requestID); ferr == nil {
		if step, ok := fresh.episodeStep(key); ok {
			return e.stepOutcome(fresh, step), nil
		}
	}
	return "", nil
}

// stepOutcome reports what a previously recorded step is currently worth.
//
// A step whose instruction is BLOCKED is surfaced rather than forgotten: the
// episode did not make progress, and saying so is the whole point.
func (e *Engine) stepOutcome(st *State, step EpisodeStep) string {
	switch step.Status {
	case StepRefused:
		return step.Reason
	case StepDispatched:
		id := step.EffectID
		if step.ArtifactCorrectionEffectID != "" {
			id = step.ArtifactCorrectionEffectID
		}
		if eff, ok := st.EffectByID(id); ok && eff.Status == EffectBlocked {
			return fmt.Sprintf("the %s instruction for phase %s is blocked and was not retried: %s",
				step.Role, step.Phase, eff.LastError)
		}
	}
	return ""
}

// ensureEpisodeRoleSession returns the confirmed session bound to a role,
// launching one through the canonical activation path if there is none.
func (e *Engine) ensureEpisodeRoleSession(ctx context.Context, requestID string, role ProfileRole) (string, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if b, ok := st.BackendFor(role); ok && b.SessionID != "" {
		if ref, exists := st.ExecutionProfiles[role]; exists {
			if err := st.AuthorizeProfileExecution(ref.Profile(), st.Workspace, e.now()); err != nil {
				return "", err
			}
		}
		return b.SessionID, nil
	}
	ref, ok := st.ExecutionProfiles[role]
	if !ok {
		return "", fmt.Errorf("no profile is bound to the %s role", role)
	}
	res, err := e.LaunchProfile(ctx, requestID, role, ref.Name, st.Workspace, "episode")
	if err != nil {
		return "", err
	}
	return res.SessionID, nil
}

// --- judgements ------------------------------------------------------------

// openEpisodeJudgement opens the independent review a phase owes.
//
// The reviewer role is the ONLY role that may fill it, and the open review names
// the pinned model for that role, so an operator editing configuration later
// cannot change which model an in-flight judgement is attributed to.
func (e *Engine) openEpisodeJudgement(
	ctx context.Context, requestID string, phase EpisodePhase, round int,
	kind ReviewKind, build func(*State) string,
) (string, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if st.ActiveReview != nil {
		// Something is already owed. Two outstanding reviews of the same work
		// produce two answers, and at least one of them is wasted.
		return "", nil
	}
	refusalKey := episodeStepKey(phase, round)
	if prior, ok := st.episodeStep(refusalKey); ok && prior.Status == StepRefused && prior.EvidenceRefusal != nil {
		return prior.Reason, nil
	}
	if rec := st.episodeReviewFor(phase, round); rec != nil {
		// Either it is still owed its answer, or it settled and the phase decision
		// owns what that verdict means. Neither is this function's business.
		return "", nil
	}

	session, err := e.ensureEpisodeRoleSession(ctx, requestID, RoleReviewer)
	if err != nil {
		return fmt.Sprintf("no independent reviewer could be given a session, so phase %s was not started: %v",
			phase, err), nil
	}

	artifactContext, promptEvidenceRemaining, err := e.episodeArtifactContextWithBudget(requestID, st)
	if err != nil {
		return "", err
	}
	var evidenceRef EvidenceRef
	if phase == PhaseImplement || phase == PhaseCompletionReview {
		ref, evidence, err := e.retainEpisodeReviewEvidence(st, promptEvidenceRemaining)
		if err != nil {
			var refusal *episodeEvidenceRefusal
			if errors.As(err, &refusal) {
				cause := e.refuseEpisodeEvidence(requestID, func(current *State) (EpisodeStep, ProfileRef, error) {
					if current.Episode == nil || current.Stage.Terminal() || current.CancellationRequested || current.Episode.Phase != string(phase) || current.Generation != st.Generation {
						return EpisodeStep{}, ProfileRef{}, fmt.Errorf("episode phase changed before evidence refusal could be recorded")
					}
					binding, ok := current.BackendForID(session)
					profile := current.ExecutionProfiles[RoleReviewer]
					if !ok || binding.Backend == "" || profile.Name == "" {
						return EpisodeStep{}, ProfileRef{}, fmt.Errorf("reviewer binding changed before evidence refusal could be recorded")
					}
					return EpisodeStep{Key: refusalKey, Phase: string(phase), Round: round, Role: string(RoleReviewer), Profile: profile.Name, Session: session, Generation: st.Generation}, profile, nil
				}, "", err)
				var commitErr *episodeEvidenceCommitError
				if errors.As(cause, &commitErr) {
					return "", cause
				}
				return cause.Error(), nil
			}
			return err.Error(), nil
		}
		evidenceRef = ref
		artifactContext += evidence
	}
	reviewID := episodeReviewID(kind, round)
	if phase == PhaseImplement {
		reviewID = fmt.Sprintf("EI%d", round)
	}
	if _, err := e.store.UpdateCommitted(requestID, func(s *State) error {
		if s.ActiveReview != nil {
			return nil
		}
		if s.Episode == nil {
			return fmt.Errorf("agent-engine: no episode is running, so no %s judgement can be owed", phase)
		}
		if s.IsImplementationSession(session) {
			return fmt.Errorf("%w: session %s implemented this episode and cannot review it",
				ErrProfileNotIsolated, session)
		}
		if evidenceRef.ID != "" {
			if err := s.AddEvidence(evidenceRef); err != nil {
				return err
			}
		}
		review := NewActiveReview(kind, reviewID, session, s.Generation, round)
		review.RequestedAt = e.stamp()
		// Assigned BEFORE the message is built: the answer instruction reads
		// ActiveReview to stamp the correlation, and building it first would stamp
		// the previous review's identity.
		s.ActiveReview = review
		s.ActiveReview.ExpectedModel = s.pinnedModelFor(RoleReviewer)

		conformance, _ := json.Marshal(s.Episode.Execution.Conformance)
		verification, _ := json.Marshal(s.Episode.Execution.Verification)
		message := build(s) + artifactContext + "\nMeasured conformance receipts: " + string(conformance) + "\nManaged verification record: " + string(verification) + reviewAnswerInstruction(s)
		effectID := EffectID(s.Revision, session, string(RoleReviewer), message)
		if err := s.RecordEffect(Effect{
			EffectID:  effectID,
			SessionID: session,
			Role:      "auditor",
			Message:   message,
			Reason:    episodeStepKey(phase, round),
			CreatedAt: review.RequestedAt,
		}); err != nil {
			return err
		}
		s.Episode.Execution.Reviews = append(s.Episode.Execution.Reviews, EpisodeReview{
			Phase: string(phase), Round: round, Kind: string(kind), ReviewID: reviewID,
			ReviewerSession: session, OpenedAt: review.RequestedAt,
		})
		s.recordEpisodeStep(EpisodeStep{
			Key: episodeStepKey(phase, round), Phase: string(phase), Round: round,
			Role: string(RoleReviewer), Profile: s.pinnedProfileName(RoleReviewer),
			Session: session, EffectID: effectID, Status: StepDispatched,
			Reason: "an independent judgement of phase " + string(phase) + " is owed", At: review.RequestedAt,
		})
		return nil
	}); err != nil {
		return "", err
	}
	if _, err := e.dispatch.Drain(ctx, requestID); err != nil {
		return fmt.Sprintf("the independent review of phase %s could not be dispatched: %v", phase, err), nil
	}
	return "", nil
}

// episodeReviewID derives a review identity that cannot collide with the shared
// C/A/D sequences, because those count a different population.
func episodeReviewID(kind ReviewKind, round int) string {
	if kind == ReviewCompletion {
		return fmt.Sprintf("EC%d", round)
	}
	return fmt.Sprintf("ER%d", round)
}

func (s *State) pinnedProfileName(role ProfileRole) string {
	if ref, ok := s.ExecutionProfiles[role]; ok {
		return ref.Name
	}
	return ""
}

// --- verification ----------------------------------------------------------

// executeEpisodeVerification runs the template's required checks, once per round.
//
// The intent is committed before any check runs. That ordering is the whole
// recovery story: a run interrupted by a crash leaves an INCOMPLETE record,
// which the next pass reports as an unknown outcome instead of quietly running
// the same checks again and appending a second set of jobs nobody can reconcile.
func (e *Engine) executeEpisodeVerification(requestID string, tpl EpisodeTemplate, hasTemplate bool, round int) (string, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if err := st.VerifyRepairBranch(); err != nil {
		return err.Error(), nil
	}
	if !hasTemplate {
		// Without an approved template there is no declared set of required checks,
		// so nothing here can establish that verification ran. An empty required set
		// would sail through the gate, and reporting that as verified would be
		// inventing the evidence the gate exists to demand.
		return "no approved template is in force, so this episode has no declared required checks and " +
			"verification cannot be established", nil
	}
	required := dedupeStrings(tpl.RequiredChecks)
	if st.Episode.PlanAuthorized {
		body, err := e.ReadEpisodeArtifact(requestID, st.Episode.PlanDigest)
		if err != nil {
			return "", err
		}
		var plan contract.ImplementationPlan
		if err := json.Unmarshal(body, &plan); err != nil {
			return "", fmt.Errorf("decode accepted implementation plan: %w", err)
		}
		for _, step := range plan.Steps {
			required = append(required, step.RequiredChecks...)
		}
		required = dedupeStrings(required)
	}
	if v := st.Episode.Execution.Verification; v != nil && v.Round == round {
		return episodeVerificationVerdict(v, round), nil
	}

	now := e.nowFn
	if err := beginEpisodeVerification(e.store, requestID, round, required, now); err != nil {
		return "", err
	}

	// The managed checks run OUTSIDE any store lock: they are real processes
	// against a pinned snapshot and they take minutes.
	receipts, rerr := e.store.runRequiredChecks(requestID, required, now)
	if rerr == nil {
		rerr = st.VerifyRepairBranch()
	}
	gate := EvaluateVerification(required, receipts, Snapshot{})
	for _, conformance := range st.Episode.Execution.Conformance {
		if len(conformance.Deviations) > 0 {
			gate.Satisfied = false
			gate.Reason = "implementation deviated from the accepted plan"
		}
	}

	reason := gate.Reason
	if rerr != nil {
		// A check that could not be run is not a pass. It is recorded as the
		// refusal it is, and the phase does not move.
		reason = rerr.Error()
	}
	var contentSHA string
	if n := len(receipts); n > 0 {
		contentSHA = receipts[n-1].ContentDigest
	}
	if err := e.store.RecordEpisodeVerification(requestID, round, required, receipts,
		contentSHA, rerr == nil && gate.Satisfied, reason, e.stamp()); err != nil {
		return "", err
	}
	if rerr != nil {
		return "the required checks could not be run, so verification is unknown: " + reason, nil
	}
	if !gate.Satisfied {
		return "verification ran and did not pass: " + reason, nil
	}
	return "", nil
}

// validateEpisodePlanPolicy checks plan declarations against the local
// admission boundary before proposal or acceptance can lead to a worker turn.
// Concrete PlannedWrites make the comparison mechanical; retained plans without
// targets remain readable and are still checked against measured diffs.
func (e *Engine) validateEpisodePlanPolicy(requestID string, plan *contract.ImplementationPlan) error {
	if plan == nil {
		return fmt.Errorf("%w: no plan supplied", contract.ErrEpisodeInvalid)
	}
	state, err := e.store.State(requestID)
	if err != nil {
		return err
	}
	if state.Episode != nil && state.Episode.ImportedPlan != nil {
		document := state.Episode.ImportedPlan
		if err := contract.ValidateImportedPlanDocument(document); err != nil {
			return err
		}
		if plan.ImportedPlanSHA256 != document.SHA256 {
			return fmt.Errorf("%w: plan must preserve the admitted imported plan sha256 %s", contract.ErrEpisodeInvalid, document.SHA256)
		}
		for _, step := range plan.Steps {
			if strings.TrimSpace(step.ImportedPlanExcerpt) == "" || !strings.Contains(document.Content, step.ImportedPlanExcerpt) {
				return fmt.Errorf("%w: step %q must quote exact text from the imported plan", contract.ErrEpisodeInvalid, step.StepID)
			}
		}
	} else if plan.ImportedPlanSHA256 != "" {
		return fmt.Errorf("%w: plan names an imported document not admitted by this episode", contract.ErrEpisodeInvalid)
	}
	for _, step := range plan.Steps {
		for _, id := range step.RequiredChecks {
			if _, err := e.store.Config().Check(id); err != nil {
				return fmt.Errorf("%w: step %q requires an unavailable check: %v", contract.ErrEpisodeInvalid, step.StepID, err)
			}
		}
	}
	request, err := e.store.loadRequestDir(requestID)
	if err != nil {
		return err
	}
	for _, step := range plan.Steps {
		for _, target := range step.PlannedWrites {
			for _, protected := range request.Request.Constraints.ProtectedPaths {
				if pathCovered(protected, target) {
					return fmt.Errorf("%w: step %q plans to write %q, protected by the admitted local request", contract.ErrEpisodeInvalid, step.StepID, target)
				}
			}
			if len(request.Request.Constraints.AllowedPaths) > 0 {
				allowed := false
				for _, scope := range request.Request.Constraints.AllowedPaths {
					allowed = allowed || pathCovered(scope, target)
				}
				if !allowed {
					return fmt.Errorf("%w: step %q plans to write %q outside the admitted local write scope", contract.ErrEpisodeInvalid, step.StepID, target)
				}
			}
		}
	}
	return nil
}

// beginEpisodeVerification commits the INTENT to run one verification round.
//
// It is separate from the observation because the two must not be able to happen
// together: the intent is what makes an interrupted run visible as an interrupted
// run. Written after the checks, it would record only successes, and a process
// that died mid-suite would leave the episode exactly where it was — free to run
// it again and again, appending jobs nobody could later pair up.
func beginEpisodeVerification(s *Store, requestID string, round int, required []string, now func() time.Time) error {
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		if st.Episode == nil {
			return fmt.Errorf("agent-engine: no episode is running, so no verification can be started")
		}
		if prev := st.Episode.Execution.Verification; prev != nil && prev.Round == round {
			return nil
		}
		st.Episode.Execution.Verification = &EpisodeVerification{
			Round: round, Required: append([]string(nil), required...),
			StartedAt: now().UTC().Format(time.RFC3339),
		}
		return nil
	})
	return err
}

// episodeVerificationVerdict renders what one recorded verification round means.
func episodeVerificationVerdict(v *EpisodeVerification, round int) string {
	if !v.Complete {
		return fmt.Sprintf("verification round %d was interrupted and its outcome is unknown; it is not "+
			"re-run automatically and is not read as a pass", round)
	}
	if v.Satisfied {
		return ""
	}
	return "verification ran and did not pass: " + v.Reason
}

// RecordEpisodeVerification commits the outcome of one verification round.
//
// It is a Store method because the verdict is state, and state is committed under
// the store lock by the one mutator rather than assembled by a caller and written
// somewhere else.
func (s *Store) RecordEpisodeVerification(
	requestID string, round int, required []string, receipts []CheckReceipt,
	contentSHA string, satisfied bool, reason, endedAt string,
) error {
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		if st.Episode == nil {
			return fmt.Errorf("agent-engine: no episode is running, so no verification result can be recorded")
		}
		started := endedAt
		if prev := st.Episode.Execution.Verification; prev != nil && prev.Round == round && prev.StartedAt != "" {
			started = prev.StartedAt
		}
		st.Episode.Execution.Verification = &EpisodeVerification{
			Round: round, Required: append([]string(nil), required...),
			Receipts: receipts, ContentSHA: contentSHA, Complete: true,
			Satisfied: satisfied, Reason: reason, EndedAt: endedAt, StartedAt: started,
		}
		return nil
	})
	return err
}

// --- plan acceptance -------------------------------------------------------

// episodeBoundProfile is one role's pinned profile identity.
type episodeBoundProfile struct {
	role ProfileRole
	ref  ProfileRef
}

// boundEpisodeProfiles lists the profiles bound to this episode, in a stable order.
//
// Sorted so a refusal about two equally-bound profiles names the same one on
// every run.
func boundEpisodeProfiles(st *State) []episodeBoundProfile {
	names := make([]string, 0, len(st.ExecutionProfiles))
	for role := range st.ExecutionProfiles {
		names = append(names, string(role))
	}
	sort.Strings(names)
	out := make([]episodeBoundProfile, 0, len(names))
	for _, n := range names {
		out = append(out, episodeBoundProfile{role: ProfileRole(n), ref: st.ExecutionProfiles[ProfileRole(n)]})
	}
	return out
}

// AcceptEpisodePlan records a LOCAL authority's acceptance of a drafted plan.
//
// It is the only path that sets PlanAuthorized, and the only place that can
// refuse it before the contract does. One refusal matters beyond the contract's
// own: an accepting authority naming a profile BOUND TO THIS EPISODE is a model
// accepting its own work. The contract already refuses the plan's own execution
// profile; this refuses every bound profile by name, so renaming the worker in
// the plan does not open the door.
//
// Nothing here consults a worker, a reviewer or any model to decide whether a
// plan may be accepted. Acceptance is a local decision, full stop.
func (e *Engine) AcceptEpisodePlan(requestID string, auth contract.PlanAuthorization, plan *contract.ImplementationPlan) (string, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if st.ProgramBinding != nil && !strings.HasPrefix(auth.AcceptedBy, "operator:") {
		return "", fmt.Errorf("agent-engine: a program-plan mismatch requires an operator: identity or an internal exact-card policy receipt")
	}
	return e.acceptEpisodePlan(requestID, auth, plan, nil)
}

func (e *Engine) acceptEpisodePlan(requestID string, auth contract.PlanAuthorization, plan *contract.ImplementationPlan, programReceipt *ProgramPlanAcceptanceReceipt) (string, error) {
	if err := contract.ValidateImplementationPlan(plan); err != nil {
		return "", err
	}
	if err := e.validateEpisodePlanPolicy(requestID, plan); err != nil {
		return "", err
	}
	if err := contract.ValidatePlanAuthorization(&auth, plan); err != nil {
		return "", err
	}
	if programReceipt != nil && auth.AcceptedBy != "policy:program:"+programReceipt.AuthorityRef {
		return "", fmt.Errorf("agent-engine: program policy acceptance identity is engine-controlled")
	}
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	if st.Episode == nil {
		return "", fmt.Errorf("agent-engine: no episode is running, so no plan can be accepted")
	}
	if st.EpisodeResumeValidation != nil && st.EpisodeResumeValidation.Blocked {
		return "", fmt.Errorf("agent-engine: episode resume is blocked; plan acceptance is refused: %s", st.EpisodeResumeValidation.Reason)
	}
	if err := st.VerifyRepairBranch(); err != nil {
		return "", err
	}
	if st.Episode.PlanDigest == "" {
		return "", fmt.Errorf("agent-engine: no plan has been proposed for this episode, so there is nothing to accept")
	}
	measured, err := plan.Digest()
	if err != nil {
		return "", err
	}
	if measured != st.Episode.PlanDigest {
		return "", fmt.Errorf("agent-engine: acceptance names a superseded or unproposed plan")
	}
	if _, err := e.ReadEpisodeArtifact(requestID, measured); err != nil {
		return "", err
	}
	for _, bound := range boundEpisodeProfiles(st) {
		if programReceipt == nil && strings.EqualFold(strings.TrimSpace(auth.AcceptedBy), bound.ref.Name) {
			return "", fmt.Errorf("%w: %q is the profile bound to the %s role of this episode, and a model may "+
				"not accept the work it produced; acceptance must be a human operator or an approved local policy",
				contract.ErrEpisodeInvalid, auth.AcceptedBy, bound.role)
		}
	}
	admitted, err := e.store.loadRequestDir(requestID)
	if err != nil {
		return "", err
	}
	writeGrant := plan.Grant == "read-write" || plan.Grant == string(CapabilityScopedWrite)
	readGrant := plan.Grant == "read-only" || plan.Grant == string(CapabilityReadOnly)
	if (!writeGrant && !readGrant) || (writeGrant && !admitted.Request.Constraints.AllowCodeChanges) {
		return "", fmt.Errorf("agent-engine: plan grant exceeds or does not identify the admitted local capability")
	}
	if worker, ok := st.ExecutionProfiles[RoleWorker]; ok && plan.ExecutionProfile != worker.Name {
		return "", fmt.Errorf("agent-engine: plan names an unadmitted execution profile")
	}
	if reviewer, ok := st.ExecutionProfiles[RoleReviewer]; ok && plan.ReviewProfile != reviewer.Name {
		return "", fmt.Errorf("agent-engine: plan names an unadmitted review profile")
	}
	baseline, err := measureStableEpisodeSource(st.Workspace, e.store.Config().StoreRoot, e.now())
	if err != nil {
		return "", err
	}
	if baseline.ContentDigest != plan.SourceBaseline || auth.SourceBaseline != plan.SourceBaseline {
		return "", fmt.Errorf("agent-engine: plan source baseline does not match the current workspace")
	}
	if reinvestigation := st.Episode.ReinvestigationBaseline; reinvestigation != nil && baseline.ContentDigest != reinvestigation.ContentDigest {
		return "", fmt.Errorf("agent-engine: workspace changed after re-investigation; new report and plan are required")
	}
	if _, err := e.store.UpdateCommitted(requestID, func(s *State) error {
		if s.Episode == nil || s.Episode.EpisodeID != st.Episode.EpisodeID || s.Episode.PlanDigest != measured || s.Episode.ReportDigest != st.Episode.ReportDigest {
			return fmt.Errorf("agent-engine: episode artifacts changed during local plan acceptance")
		}
		if s.EpisodeResumeValidation != nil && s.EpisodeResumeValidation.Blocked {
			return fmt.Errorf("agent-engine: episode resume became blocked before plan acceptance")
		}
		if s.Episode.ReinvestigationBaseline != nil && (s.Episode.ReinvestigationBaseline.ContentDigest != baseline.ContentDigest || s.Episode.ReinvestigationBaseline.ContentDigest != plan.SourceBaseline) {
			return fmt.Errorf("agent-engine: active re-investigation baseline changed before plan acceptance")
		}
		if err := s.AuthorizePlan(auth, plan, e.nowFn()); err != nil {
			return err
		}
		if programReceipt != nil {
			if s.ProgramBinding == nil || s.ProgramBinding.ProgramDigest != programReceipt.ProgramDigest ||
				s.ProgramBinding.CardDigest != programReceipt.CardDigest || s.ProgramBinding.CardID != programReceipt.CardID ||
				programReceipt.ProgramID != s.ProgramBinding.Definition.ProgramID ||
				programReceipt.AuthorityRef != s.ProgramBinding.AuthorityRef ||
				programReceipt.PlanDigest != measured || !programAuthorityLive(s.ProgramBinding, e.nowFn()) {
				return fmt.Errorf("agent-engine: local program acceptance receipt no longer matches the admitted authority")
			}
			card, ok := programCard(&s.ProgramBinding.Definition, s.ProgramBinding.CardID)
			if !ok || !sameProgramRoles(card.Roles, programRolesFromProfiles(s.ExecutionProfiles)) || s.EpisodeTemplate != card.Template {
				return fmt.Errorf("agent-engine: local role, model or template binding changed before program acceptance")
			}
			template, exists := e.store.Config().EpisodeTemplates.Lookup(s.EpisodeTemplate)
			templateDigest, digestErr := programTemplateDigest(template)
			if !exists || digestErr != nil || templateDigest != s.ProgramBinding.TemplateDigest {
				return fmt.Errorf("agent-engine: local template changed before program acceptance")
			}
			s.ProgramAcceptances = append(s.ProgramAcceptances, *programReceipt)
		}
		s.Episode.PlanBaseline = &baseline
		s.Episode.ReinvestigationBaseline = nil
		s.Episode.PrechargedReportRound = 0
		s.Episode.InvalidatedReason = ""
		return nil
	}); err != nil {
		return "", err
	}
	return plan.Digest()
}

// --- prompts ---------------------------------------------------------------

// episodeStepPrompt is the instruction a role receives for a phase.
//
// Every one of them states what the role may NOT do, because a role asked to help
// with an episode otherwise drifts into deciding the episode: the investigator
// proposing the plan, the implementer declaring its own work done, the planner
// accepting its own plan.
func episodeStepPrompt(st *State, phase EpisodePhase, key, objective string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Episode %s (%s) is at phase %s.\n\n", st.Episode.EpisodeID, key, phase)
	fmt.Fprintf(&b, "Objective: %s\n", objective)
	if st.Episode.ReportDigest != "" {
		fmt.Fprintf(&b, "Investigation report on record: %s\n", brief(st.Episode.ReportDigest))
	}
	if st.Episode.PlanDigest != "" {
		fmt.Fprintf(&b, "Plan on record: v%d %s\n", st.Episode.PlanVersion, brief(st.Episode.PlanDigest))
	}
	switch phase {
	case PhaseReport:
		b.WriteString("\nInvestigate the workspace and produce a SEMANTIC REPORT for this episode: the findings " +
			"you can attribute to evidence, and an explicit UNKNOWN for everything you cannot.\n")
		b.WriteString("You do not plan, you do not implement, and you do not judge your own findings. Submit the " +
			"report through Agent Engine; do not edit control state.\n")
		if st.Episode.ImportedPlan != nil {
			b.WriteString("An existing implementation plan was imported. Investigate its assumptions and compatibility with the actual workspace and admitted obligations; preserve its scope and disclose conflicts or missing evidence. Do not draft a replacement plan.\n")
		}
	case PhasePlan:
		if st.Episode.ImportedPlan != nil {
			fmt.Fprintf(&b, "\nReview the IMPORTED IMPLEMENTATION PLAN (source sha256 %s) against the accepted report, current source and admitted obligations. Preserve its design, ordered work, requirements, limits and acceptance criteria. Do not draft a replacement, add work or silently omit a requirement. Map the existing plan into the ImplementationPlan JSON shape, using local finding/obligation IDs, profiles and registered checks. Each step must quote exact original text in imported_plan_excerpt. This mapping is not permission to redesign the plan.\n", st.Episode.ImportedPlan.SHA256)
			b.WriteString("If the existing plan conflicts with the source, obligations or local policy, or cannot be represented faithfully, return the correlated EpisodeArtifactEnvelope with imported_plan_refusal describing the specific conflict and no plan/report. The Engine will stop for the caller to revise the source plan, not replace it automatically.\n")
		} else {
			b.WriteString("\nDraft an IMPLEMENTATION PLAN answering the report on record, naming the steps, the " +
				"allowed change kinds and the acceptance criteria for each.\n")
		}
		b.WriteString("A plan is a proposal. It is not accepted until a local authority accepts it, and you may " +
			"not accept it yourself.\n")
	case PhaseImplement:
		if st.Episode.ImportedPlan != nil {
			b.WriteString("\nThe original imported plan is supplied with its exact content digest alongside the accepted structured mapping. Follow both. If their requirements conflict or a required source-plan step is absent from the mapping, stop and report the discrepancy; do not choose a replacement plan.\n")
		}
		if len(st.RepairBundleTargets) > 0 {
			b.WriteString("\nRepair bundle is bound to these locally prepared worktrees:\n")
			for _, target := range st.RepairBundleTargets {
				fmt.Fprintf(&b, "- %s/: branch %s, base %s\n", target.ID, target.Branch, target.BaseHead)
			}
			b.WriteString("Keep each change inside its target directory and the accepted plan. Do not switch branches, merge, push, commit, or edit original checkouts.\n")
		} else if st.RepairBranch != "" {
			fmt.Fprintf(&b, "\nRepair branch: %s (base %s). Work only in this worktree; do not switch branch, merge, push, commit, or edit the original checkout.\n", st.RepairBranch, st.RepairBaseHead)
		}
		b.WriteString("\nImplement the ACCEPTED plan, and only the accepted plan, inside the workspace. Its " +
			"recorded digest is the authority: if the plan you are given does not match, stop and say so.\n")
		b.WriteString("Do not declare the episode complete. Completion is judged by an independent reviewer " +
			"against executed checks, and you may not open that judgement yourself.\n")
		b.WriteString("Use this admitted worker session's own file and shell tools synchronously. Do not delegate implementation or required shell commands to Task, sub-agent, agent or background-session tools: another session's claimed result cannot establish this session's execution. If your own tools cannot perform the work, report the blocker instead.\n")
	}
	b.WriteString("\nRequest ID: " + st.RequestID + ". Do not edit control state.\n")
	return b.String()
}

// reportReviewPrompt asks the reviewer whether the investigation stands.
//
// It is a CHECKPOINT judgement and therefore cannot approve anything: the
// reviewer is asked whether the report is good enough to plan against, not
// whether the episode is finished.
func reportReviewPrompt(st *State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An independent review of episode %s's investigation report is owed.\n\n", st.Episode.EpisodeID)
	fmt.Fprintf(&b, "Report digest: %s\n", st.Episode.ReportDigest)
	fmt.Fprintf(&b, "Produced by profile %s (%s).\n\n", st.Episode.ReportProducer, st.Episode.ReportModel)
	b.WriteString("Judge the REPORT, not the implementer: is what it claims established actually attributable to " +
		"the evidence it names, and is every unknown stated as an unknown?\n")
	b.WriteString("Answer with one of: CONTINUE (the report stands and may be planned against), " +
		"COLLECT_EVIDENCE or REDIRECT (the report must be revised before planning), DIAGNOSE, REFRESH, BLOCKED.\n")
	b.WriteString("You may not approve this episode from a report review, and you did not produce the report.\n")
	if st.Episode.ImportedPlan != nil {
		b.WriteString("An existing plan was imported. Check that the report actually investigated that plan's source assumptions and compatibility. Unsupported or conflicting assumptions must remain unresolved; the report cannot silently replace the original requirements.\n")
	}
	return b.String()
}

// completionReviewPrompt asks the reviewer for the independent completion audit.
//
// It states plainly what the audit is for: the reviewer is asked whether the
// episode is actually finished, and the checks recorded below are evidence for
// that judgement rather than a substitute for it.
func completionReviewPrompt(st *State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An independent completion audit is owed for episode %s.\n\n", st.Episode.EpisodeID)
	fmt.Fprintf(&b, "Plan: v%d %s, accepted by %s.\n", st.Episode.PlanVersion,
		brief(st.Episode.PlanDigest), st.Episode.AuthorizedBy)
	b.WriteString("\nObligations:\n")
	for _, o := range st.Obligations {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", o.Disposition, o.ID, o.Criterion)
	}
	if v := st.Episode.Execution.Verification; v != nil && v.Complete {
		fmt.Fprintf(&b, "\nManaged checks ran for round %d: satisfied=%t %s\n", v.Round, v.Satisfied, v.Reason)
	}
	b.WriteString("\nJudge whether the episode is actually complete against its obligations and its executed " +
		"checks. A green command, a commit or a confident report is not evidence.\n")
	if st.Episode.ImportedPlan != nil {
		b.WriteString("Also judge against the complete original imported plan. The structured mapping must not have silently omitted, replaced or weakened its requirements; a missing requirement cannot be excused because it was absent from that mapping.\n")
	}
	b.WriteString("Answer with one of: PASS, FAIL, BLOCKED. Only a PASS here may verify the episode, and only " +
		"while the required receipts are current.\n")
	return b.String()
}

func (s *Store) AcceptEpisodePlan(requestID string, auth contract.PlanAuthorization, plan *contract.ImplementationPlan) (string, error) {
	return (&Engine{store: s, now: s.now}).AcceptEpisodePlan(requestID, auth, plan)
}
