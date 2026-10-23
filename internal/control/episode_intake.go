package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/agentdeck"
)

// EpisodeArtifactEnvelope carries content, never attribution or authority.
// Session and model identity are supplied by the transport which was queried.
type EpisodeArtifactEnvelope struct {
	RequestID           string                       `json:"request_id"`
	StepKey             string                       `json:"step_key"`
	ArtifactNonce       string                       `json:"artifact_nonce"`
	Generation          int                          `json:"generation"`
	ModelEcho           string                       `json:"model_echo,omitempty"`
	Report              *contract.SemanticReport     `json:"report,omitempty"`
	Plan                *contract.ImplementationPlan `json:"plan,omitempty"`
	ImportedPlanRefusal string                       `json:"imported_plan_refusal,omitempty"`
}

func episodeArtifactInstruction(id, key, nonce string, generation int) string {
	return fmt.Sprintf("\nReturn exactly one JSON EpisodeArtifactEnvelope, with request_id=%q, step_key=%q, artifact_nonce=%q, generation=%d. Include report (SemanticReport) for REPORT or plan (ImplementationPlan) for PLAN. Do not invoke the control CLI. Attribution is measured independently; this envelope grants no permission.\n", id, key, nonce, generation)
}

func decodeEpisodeArtifact(text string) (EpisodeArtifactEnvelope, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json") && strings.HasSuffix(text, "```") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```"))
	}
	var env EpisodeArtifactEnvelope
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return env, err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return env, fmt.Errorf("multiple artifact envelopes refused")
	} else if !errors.Is(err, io.EOF) {
		return env, err
	}
	return env, nil
}

func (e *Engine) collectEpisodeOutput(ctx context.Context, id string, st *State, step EpisodeStep) error {
	if st.Stage.Terminal() || step.Status == StepRefused {
		return nil // A durable refusal is not another transport read opportunity.
	}
	binding, ok := st.BackendForID(step.Session)
	if !ok {
		return fmt.Errorf("episode artifact producer is no longer bound")
	}
	if binding.Backend != "agent-deck" {
		return nil
	} // ACP uses durable terminal receipts.
	out, err := e.runtime.Output(ctx, step.Session, 16000)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out.Text) == "" {
		return nil
	}
	if out.SessionID != step.Session || out.Truncated {
		return fmt.Errorf("episode artifact output identity is mismatched or truncated")
	}
	if profile := st.ExecutionProfiles[ProfileRole(step.Role)]; profile.Tool == "codex" && out.NativeTurn == "" {
		return nil // A partial pane cannot supply completed producer evidence.
	}
	if err := e.intakeEpisodeArtifactFromTurn(id, binding.Backend, out.SessionID, "", out.Model, out.NativeTurn, out.Text, out.Evidence); err != nil {
		if handled, correctionErr := e.correctEpisodeArtifactNonce(ctx, id, step, out); handled {
			return correctionErr
		}
		return err
	}
	return nil
}

// reconcileEpisodeProducer checks retained producer output before deciding a
// REPORT or PLAN producer has failed. A valid correlated artifact wins even if
// the session has since stopped; only a positively observed terminal session
// with no committed artifact produces a durable BLOCKED result.
func (e *Engine) reconcileEpisodeProducer(ctx context.Context, id string, snapshot *State, step EpisodeStep) (string, error) {
	collectErr := e.collectEpisodeOutput(ctx, id, snapshot, step)
	var commitErr *episodeEvidenceCommitError
	if errors.As(collectErr, &commitErr) {
		return "", collectErr
	}
	current, err := e.store.State(id)
	if err != nil {
		return "", err
	}
	if current.Stage.Terminal() || current.CancellationRequested || current.Episode == nil {
		return "", nil
	}
	if producerArtifactDigest(current, step.Phase) != "" {
		// Intake committed the digest only after correlation, contract validation
		// and durable artifact retention all succeeded.
		return "", nil
	}
	backend, ref, err := e.dispatch.backendFor(id, step.Session)
	if err != nil {
		return "", err
	}
	session, inspectErr := backend.Inspect(ctx, ref)
	if inspectErr != nil {
		if agentdeck.IsNotFound(inspectErr) {
			// A transport's typed not-found response is a positive observation,
			// unlike a timeout or an unreadable session list.
			session.Status = "missing"
		} else {
			if collectErr != nil {
				return "retained producer output has not been accepted: " + collectErr.Error(), nil
			}
			return "", inspectErr
		}
	}
	status := strings.ToLower(strings.TrimSpace(session.Status))
	switch status {
	case "stopped", "dead", "exited", "closed", "error", "errored", "stale", "missing":
	default:
		if collectErr != nil {
			return "retained producer output has not been accepted: " + collectErr.Error(), nil
		}
		return "", nil
	}
	reason := fmt.Sprintf("the %s producer session %s reported %s and no valid correlated %s artifact was retained",
		step.Role, step.Session, status, step.Phase)
	if collectErr != nil {
		reason += "; retained output was refused: " + collectErr.Error()
	}
	_, err = e.store.UpdateCommitted(id, func(s *State) error {
		if s.Stage.Terminal() || s.CancellationRequested || s.Episode == nil ||
			s.Episode.Phase != step.Phase || s.Generation != step.Generation {
			return nil
		}
		fresh, ok := s.episodeStep(step.Key)
		if !ok || fresh.Session != step.Session || fresh.EffectID != step.EffectID ||
			fresh.Generation != step.Generation || fresh.ArtifactDigest != "" || producerArtifactDigest(s, step.Phase) != "" {
			return nil
		}
		_, _, stopErr := episodeStop(s, e.now(), reason)
		return stopErr
	})
	if err != nil {
		return "", err
	}
	latest, err := e.store.State(id)
	if err != nil {
		return "", err
	}
	if latest.Episode != nil && latest.Episode.Stop != nil {
		if err := e.store.publishEpisodeStop(id, latest.Episode.Stop); err != nil {
			return "", err
		}
		return reason, nil
	}
	return "", nil
}

func producerArtifactDigest(s *State, phase string) string {
	if s == nil || s.Episode == nil {
		return ""
	}
	switch phase {
	case string(PhaseReport):
		return s.Episode.ReportDigest
	case string(PhasePlan):
		return s.Episode.PlanDigest
	default:
		return ""
	}
}

// Request one bounded regeneration of a completed, otherwise phase-addressed
// Codex envelope. Never repair its bytes or nonce in the controller. Other
// malformed/stale answers remain refused, and no second session is created.
func (e *Engine) correctEpisodeArtifactNonce(ctx context.Context, id string, step EpisodeStep, out agentdeck.Output) (bool, error) {
	env, err := decodeEpisodeArtifact(out.Text)
	if err != nil || out.NativeTurn == "" || out.Truncated || out.SessionID != step.Session || env.RequestID != id || env.StepKey != step.Key || env.Generation != step.Generation || env.ArtifactNonce == step.ArtifactNonce {
		return false, nil
	}
	if (step.Phase != string(PhaseReport) && step.Phase != string(PhasePlan)) || step.ArtifactNonce == "" {
		return false, nil
	}
	handled := false
	fresh, err := e.store.UpdateCommitted(id, func(s *State) error {
		current, ok := s.episodeStep(step.Key)
		binding, bound := s.BackendForID(step.Session)
		profile := s.ExecutionProfiles[ProfileRole(step.Role)]
		if !ok || !bound || binding.Backend != "agent-deck" || profile.Tool != "codex" || (profile.Model != "" && out.Model != "" && profile.Model != out.Model) || s.Stage.Terminal() || s.CancellationRequested || s.Episode.Phase != step.Phase || step.Key != episodeStepKey(EpisodePhase(s.Episode.Phase), s.episodePhaseRound(EpisodePhase(s.Episode.Phase))) || s.Generation != step.Generation || current.Session != step.Session || current.ArtifactNonce != step.ArtifactNonce || current.ArtifactDigest != "" {
			return nil
		}
		handled = true
		if current.ArtifactCorrectionEffectID != "" {
			if current.ArtifactCorrectionTurn != out.NativeTurn {
				current.Status, current.Reason = StepRefused, "artifact nonce correction budget exhausted; no artifact was accepted"
				if err := e.store.writeResultBody(id, contract.Result{Status: contract.StatusBlocked, Summary: current.Reason, Reason: current.Reason}); err != nil {
					return err
				}
				digest, err := e.store.digestResultBody(id)
				if err != nil {
					return err
				}
				s.ResultDigest, s.ResultStatus = digest, string(contract.StatusBlocked)
				s.replaceEpisodeStep(current)
				return s.Transition(StageBlocked, current.Reason, e.stamp())
			}
			return nil // Same completed refusal replayed while correction is owed.
		}
		effect, exists := s.EffectByID(current.EffectID)
		if !exists || effect.Status != EffectDelivered {
			handled = false
			return nil
		}
		message := "Your completed artifact was refused because its artifact_nonce differs from the instruction. Regenerate the envelope with the exact nonce below, preserving the same report or proposed plan, obligations and scope. Do not edit files or control state. This is the only correlation correction allowed for this phase attempt.\n" + episodeArtifactInstruction(id, step.Key, step.ArtifactNonce, step.Generation)
		effectID := EffectID(s.Revision, step.Session, step.Role, message)
		if err := s.RecordEffect(Effect{EffectID: effectID, SessionID: step.Session, Role: step.Role, Message: message, Reason: step.Key + " artifact nonce correction", CreatedAt: e.stamp()}); err != nil {
			return err
		}
		current.ArtifactCorrectionEffectID, current.ArtifactCorrectionTurn = effectID, out.NativeTurn
		s.replaceEpisodeStep(current)
		return nil
	})
	if err != nil || !handled || fresh.Stage.Terminal() {
		return handled, err
	}
	_, err = e.dispatch.Drain(ctx, id)
	return true, err
}

func (e *Engine) intakeEpisodeArtifact(id, backend, session, sendID, model, text string, evidence ...json.RawMessage) error {
	return e.intakeEpisodeArtifactFromTurn(id, backend, session, sendID, model, "", text, evidence...)
}

func (e *Engine) intakeEpisodeArtifactFromTurn(id, backend, session, sendID, model, nativeTurn, text string, evidence ...json.RawMessage) error {
	return e.intakeEpisodeArtifactFromTurnMode(id, backend, session, sendID, model, nativeTurn, text, false, evidence...)
}

// intakeEpisodeArtifactFromTurnForResumeRecovery accepts only the already
// dispatched read-only producer artifact whose old turn is being reconciled
// during a source-drift resume. The caller must first persist the
// awaiting_readonly_quiescence marker. All normal session, nonce, generation,
// send receipt, model and baseline checks remain in the shared intake path.
func (e *Engine) intakeEpisodeArtifactFromTurnForResumeRecovery(id, backend, session, sendID, model, nativeTurn, text string, evidence ...json.RawMessage) error {
	return e.intakeEpisodeArtifactFromTurnMode(id, backend, session, sendID, model, nativeTurn, text, true, evidence...)
}

func (e *Engine) intakeEpisodeArtifactFromTurnMode(id, backend, session, sendID, model, nativeTurn, text string, resumeRecovery bool, evidence ...json.RawMessage) error {
	env, err := decodeEpisodeArtifact(text)
	if err != nil {
		return err
	}
	if env.RequestID != id {
		return fmt.Errorf("artifact belongs to a different request")
	}
	st, err := e.store.State(id)
	if err != nil {
		return err
	}
	validate := func(s *State) (EpisodeStep, ProfileRef, error) {
		if s.Episode == nil || s.Stage.Terminal() || s.CancellationRequested {
			return EpisodeStep{}, ProfileRef{}, fmt.Errorf("episode cannot receive artifacts")
		}
		step, ok := s.episodeStep(env.StepKey)
		if !ok || step.Key != episodeStepKey(EpisodePhase(s.Episode.Phase), s.episodePhaseRound(EpisodePhase(s.Episode.Phase))) || step.Session != session || step.ArtifactNonce == "" || env.ArtifactNonce != step.ArtifactNonce || env.Generation != step.Generation || step.Generation != s.Generation {
			return step, ProfileRef{}, fmt.Errorf("artifact has stale or uncorrelated phase, session, nonce or generation")
		}
		if s.EpisodeResumeValidation != nil && s.EpisodeResumeValidation.Blocked {
			recoveryStep := resumeRecovery &&
				s.EpisodeResumeValidation.Resolution == "awaiting_readonly_quiescence" &&
				s.EpisodeResumeValidation.DriftObserved &&
				(step.Phase == string(PhaseReport) || step.Phase == string(PhasePlan)) &&
				step.Status == StepDispatched && step.ArtifactDigest == "" &&
				!hasDispatchedImplementation(s)
			if !recoveryStep {
				return EpisodeStep{}, ProfileRef{}, fmt.Errorf("episode resume is blocked; artifact intake is refused: %s", s.EpisodeResumeValidation.Reason)
			}
		}
		binding, ok := s.BackendForID(session)
		if !ok || binding.Backend != backend {
			return step, ProfileRef{}, fmt.Errorf("artifact transport no longer owns its producer")
		}
		profile, ok := s.ExecutionProfiles[ProfileRole(step.Role)]
		if !ok {
			return step, profile, fmt.Errorf("artifact role is not admitted")
		}
		if profile.Model != "" && model != "" && model != profile.Model {
			return step, profile, fmt.Errorf("artifact lacks matching answer-turn model attribution (expected %q, observed %q)", profile.Model, model)
		}
		if sendID != "" {
			correlated := false
			for _, effect := range s.Effects {
				if effect.EffectID == step.EffectID && effect.SendID == sendID {
					correlated = true
				}
			}
			if !correlated {
				return step, profile, fmt.Errorf("artifact terminal receipt is not the phase dispatch receipt")
			}
		}
		return step, profile, nil
	}
	step, profile, err := validate(st)
	if err != nil {
		return err
	}
	if env.ImportedPlanRefusal != "" {
		if step.Phase != string(PhasePlan) || st.Episode.ImportedPlan == nil || env.Plan != nil || env.Report != nil || strings.TrimSpace(env.ImportedPlanRefusal) == "" || len(env.ImportedPlanRefusal) > 8192 {
			return fmt.Errorf("imported_plan_refusal requires only a bounded reason from the admitted imported-plan review")
		}
		body, err := json.Marshal(env)
		if err != nil {
			return err
		}
		digest := sha256Hex(body)
		if err := e.retainEpisodeArtifact(id, digest, env); err != nil {
			return e.refuseEpisodeEvidence(id, validate, nativeTurn, err)
		}
		evidenceDigest, err := e.retainEpisodeProducerEvidence(id, digest, backend, step, evidence)
		if err != nil {
			return e.refuseEpisodeEvidence(id, validate, nativeTurn, err)
		}
		committed, err := e.store.UpdateCommitted(id, func(s *State) error {
			current, _, err := validate(s)
			if err != nil {
				return err
			}
			if current.ArtifactDigest != "" || s.Episode.PlanDigest != "" {
				return fmt.Errorf("a completed plan attempt cannot be replaced with an import refusal")
			}
			current.Status, current.Reason = StepRefused, "imported plan needs revision: "+env.ImportedPlanRefusal
			current.ArtifactDigest, current.ArtifactEvidenceDigest = digest, evidenceDigest
			s.replaceEpisodeStep(current)
			_, _, err = episodeStop(s, e.now(), current.Reason)
			return err
		})
		if err != nil {
			return err
		}
		return e.store.publishEpisodeStop(id, committed.Episode.Stop)
	}
	var artifact any
	var digest string
	switch EpisodePhase(step.Phase) {
	case PhaseReport:
		if env.Report == nil || env.Plan != nil {
			return fmt.Errorf("REPORT requires only a semantic report")
		}
		env.Report.ObservedProfile, env.Report.ObservedModel = profile.Name, model
		if env.Report.EpisodeID != st.Episode.EpisodeID {
			return fmt.Errorf("report belongs to a different episode")
		}
		if err := contract.ValidateSemanticReport(env.Report); err != nil {
			return err
		}
		if baseline := st.Episode.ReinvestigationBaseline; baseline != nil {
			if env.Report.SourceDigest != baseline.ContentDigest {
				return fmt.Errorf("re-investigation report does not cite the active measured source baseline")
			}
			measured, measureErr := measureStableEpisodeSource(st.Workspace, e.store.Config().StoreRoot, e.now())
			if measureErr != nil {
				return measureErr
			}
			if measured.ContentDigest != baseline.ContentDigest {
				return fmt.Errorf("workspace drifted from the re-investigation baseline before report intake")
			}
		}
		digest, err = env.Report.Digest()
		artifact = env.Report
	case PhasePlan:
		if env.Plan == nil || env.Report != nil {
			return fmt.Errorf("PLAN requires only an implementation plan")
		}
		if env.Plan.EpisodeID != st.Episode.EpisodeID || env.Plan.AcceptedReportDigest != st.Episode.ReportDigest {
			return fmt.Errorf("plan answers a different episode or report")
		}
		if worker, ok := st.ExecutionProfiles[RoleWorker]; ok && env.Plan.ExecutionProfile != worker.Name {
			return fmt.Errorf("plan names an unadmitted execution profile")
		}
		if reviewer, ok := st.ExecutionProfiles[RoleReviewer]; ok && env.Plan.ReviewProfile != reviewer.Name {
			return fmt.Errorf("plan names an unadmitted review profile")
		}
		var baseline Snapshot
		var err error
		if st.Episode.ReinvestigationBaseline != nil {
			baseline, err = measureStableEpisodeSource(st.Workspace, e.store.Config().StoreRoot, e.now())
		} else {
			baseline, err = TakeSnapshot(st.Workspace, e.nowFn(), e.store.Config().StoreRoot)
		}
		if err != nil {
			return err
		}
		if env.Plan.SourceBaseline != baseline.ContentDigest {
			return fmt.Errorf("plan baseline differs from the measured workspace")
		}
		if reinvestigation := st.Episode.ReinvestigationBaseline; reinvestigation != nil && baseline.ContentDigest != reinvestigation.ContentDigest {
			return fmt.Errorf("workspace drifted from the active re-investigation baseline before plan intake")
		}
		if err := contract.ValidateImplementationPlan(env.Plan); err != nil {
			return err
		}
		if err := e.validateEpisodePlanPolicy(id, env.Plan); err != nil {
			return err
		}
		digest, err = env.Plan.Digest()
		artifact = env.Plan
	default:
		return fmt.Errorf("phase does not accept an artifact")
	}
	if err != nil {
		return err
	}
	if err := e.retainEpisodeArtifact(id, digest, artifact); err != nil {
		return e.refuseEpisodeEvidence(id, validate, nativeTurn, err)
	}
	evidenceDigest, err := e.retainEpisodeProducerEvidence(id, digest, backend, step, evidence)
	if err != nil {
		return e.refuseEpisodeEvidence(id, validate, nativeTurn, err)
	}
	_, err = e.store.UpdateCommitted(id, func(s *State) error {
		current, _, err := validate(s)
		if err != nil {
			return err
		}
		if current.ArtifactDigest != "" {
			if current.ArtifactDigest != digest {
				return fmt.Errorf("phase already accepted different artifact bytes")
			}
			return nil
		}
		if env.Report != nil {
			if baseline := s.Episode.ReinvestigationBaseline; baseline != nil && env.Report.SourceDigest != baseline.ContentDigest {
				return fmt.Errorf("report source no longer matches the active re-investigation baseline")
			}
			if s.Episode.ReportDigest != "" && s.Episode.ReportDigest != digest {
				s.InvalidatePlan("investigation report changed")
			}
			s.Episode.ReportDigest, s.Episode.ReportProducer, s.Episode.ReportModel = digest, profile.Name, model
		} else {
			if env.Plan.AcceptedReportDigest != s.Episode.ReportDigest {
				return fmt.Errorf("report changed during plan intake")
			}
			if baseline := s.Episode.ReinvestigationBaseline; baseline != nil && env.Plan.SourceBaseline != baseline.ContentDigest {
				return fmt.Errorf("plan source no longer matches the active re-investigation baseline")
			}
			s.Episode.PlanDigest, s.Episode.PlanVersion, s.Episode.PlanAuthorized = digest, env.Plan.Version, false
		}
		for i := range s.Episode.Execution.Steps {
			if s.Episode.Execution.Steps[i].Key == current.Key {
				s.Episode.Execution.Steps[i].ArtifactDigest = digest
				s.Episode.Execution.Steps[i].ArtifactEvidenceDigest = evidenceDigest
				s.Episode.Execution.Steps[i].ModelEcho = env.ModelEcho
			}
		}
		return nil
	})
	return err
}

// Persist only deterministic retention-budget failures after the envelope and
// binding have been validated. I/O failures remain retryable. Refusal and its
// terminal result are committed together; an error must not roll them back.
func (e *Engine) refuseEpisodeEvidence(id string, validate func(*State) (EpisodeStep, ProfileRef, error), nativeTurn string, cause error) error {
	var refusal *episodeEvidenceRefusal
	if !errors.As(cause, &refusal) {
		return cause
	}
	_, err := e.store.UpdateCommitted(id, func(s *State) error {
		step, _, err := validate(s)
		if err != nil {
			return err
		}
		step.Status, step.Reason = StepRefused, cause.Error()
		step.EvidenceRefusal = &EpisodeEvidenceRejection{Phase: step.Phase, StepKey: step.Key, Scope: refusal.Scope, Actual: refusal.Actual, Limit: refusal.Limit, Digest: refusal.Digest, NativeTurn: nativeTurn}
		if err := e.store.writeResultBody(id, contract.Result{Status: contract.StatusBlocked, Summary: step.Reason, Reason: step.Reason}); err != nil {
			return err
		}
		digest, err := e.store.digestResultBody(id)
		if err != nil {
			return err
		}
		s.ResultDigest, s.ResultStatus = digest, string(contract.StatusBlocked)
		if _, exists := s.episodeStep(step.Key); exists {
			s.replaceEpisodeStep(step)
		} else {
			s.recordEpisodeStep(step)
		}
		return s.Transition(StageBlocked, step.Reason, e.stamp())
	})
	if err != nil {
		return &episodeEvidenceCommitError{Refusal: cause, Err: err}
	}
	return cause
}

// Shapes travel with the instruction: a confined agent cannot be expected to
// read this controller's source tree to discover its output contract.
func (e *Engine) episodeArtifactSchema(requestID string, s *State, phase EpisodePhase) (string, error) {
	var shape any
	var rules string
	if phase == PhaseReport {
		shape = contract.SemanticReport{Schema: contract.EpisodeVersion, EpisodeID: s.Episode.EpisodeID, Attempt: s.episodePhaseRound(phase), Objective: "the actual objective", EvidenceDigest: "digest of recorded evidence", SourceDigest: "measured source baseline from this instruction", Findings: []contract.ConfirmedFinding{{FindingID: "F1", RequirementID: "an actual obligation id", Expected: "expected behavior", Observed: "measured behavior", CausalRefs: []string{"actual source locations"}, EvidenceRefs: []string{"actual evidence refs"}, Unknowns: []string{"what remains unproven"}}}}
	} else {
		admitted, err := e.store.loadRequestDir(requestID)
		if err != nil {
			return "", err
		}
		grant := string(CapabilityReadOnly)
		if admitted.Request.Constraints.AllowCodeChanges {
			grant = string(CapabilityScopedWrite)
		}
		planShape := contract.ImplementationPlan{Schema: contract.EpisodeVersion, EpisodeID: s.Episode.EpisodeID, Version: s.Episode.PlanVersion + 1, AcceptedReportDigest: s.Episode.ReportDigest, SourceBaseline: "measured source baseline from this instruction", Grant: grant, ExecutionProfile: s.pinnedProfileName(RoleWorker), ReviewProfile: s.pinnedProfileName(RoleReviewer), Steps: []contract.PlanStep{{StepID: "S1", Findings: []string{"actual report finding ids"}, RequirementIDs: []string{"actual obligation ids"}, AllowedPaths: admitted.Request.Constraints.AllowedPaths, ProtectedPaths: admitted.Request.Constraints.ProtectedPaths, AllowedChangeKinds: []string{"source"}, RequiredChecks: s.Checks, AcceptanceCriteria: "falsifiable criterion", Expected: "expected behavior"}}}
		if document := s.Episode.ImportedPlan; document != nil {
			planShape.ImportedPlanSHA256 = document.SHA256
			planShape.Steps[0].ImportedPlanExcerpt = "exact text from the imported source document for this step"
		}
		shape = planShape
		constraints, _ := json.Marshal(admitted.Request.Constraints)
		rules = "\nAdmitted local constraints (scope ceilings, not current permission to write): " + string(constraints) + "\n" +
			"Use the exact grant and profile names supplied above. Narrow allowed_paths to the files each mutation step needs; never include protected paths as writable scope. " +
			"Every step must declare at least one allowed_change_kind; do not create read-only preflight, verification or review steps with an empty list. Describe those conditions in the mutation step's acceptance_criteria and required_checks instead. " +
			"Derive acceptance criteria from the admitted objective and obligations. Do not invent additional security qualification, operating-system attestation or human UI inspection gates. Native tool observations support functional execution claims when paired invocation/result records are available; managed checks establish the independently tested behavior. Human inspection of retained chats after quiescence is an operator follow-up, not a condition the model may invent for its completion verdict. Preserve any such condition explicitly required by the admitted request. " +
			"Verification and independent review remain controller-owned; the plan cannot assign itself acceptance, permissions or a completion verdict. The planner remains read-only even when proposing scoped-write work.\n"
	}
	body, _ := json.Marshal(shape)
	obligations, _ := json.Marshal(s.Obligations)
	return "\nArtifact JSON shape (replace explanatory placeholders with actual evidence; never invent evidence):\n" + string(body) + "\nCurrent obligations: " + string(obligations) + "\n" + rules, nil
}
