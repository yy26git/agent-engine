package control

import (
	"encoding/json"
	"fmt"
)

// Metadata only: authoritative plans/evidence remain in their artifact store.
const MaxProgressUpdates = 256

type ProgressView struct {
	EpisodeID                     string `json:"episode_id"`
	Phase                         string `json:"phase"`
	Stage                         Stage  `json:"stage"`
	Reason                        string `json:"reason,omitempty"`
	Terminal                      bool   `json:"terminal"`
	WaitingForOperator            bool   `json:"waiting_for_operator"`
	RequiresAction                bool   `json:"requires_action"`
	NextAction                    string `json:"next_action"`
	ActionKind                    string `json:"action_kind,omitempty"`
	PlanDigest                    string `json:"plan_digest,omitempty"`
	ImportedPlanSHA256            string `json:"imported_plan_sha256,omitempty"`
	PlanAcceptanceSource          string `json:"plan_acceptance_source,omitempty"`
	ProgramID                     string `json:"program_id,omitempty"`
	ProgramCardID                 string `json:"program_card_id,omitempty"`
	ProgramDigest                 string `json:"program_digest,omitempty"`
	ProgramCardDigest             string `json:"program_card_digest,omitempty"`
	ProgramAcceptancePlanDigest   string `json:"program_acceptance_plan_sha256,omitempty"`
	ProgramAcceptanceAuthorityRef string `json:"program_acceptance_authority_ref,omitempty"`
	ProgramAcceptanceAt           string `json:"program_acceptance_at,omitempty"`
	ArtifactReference             string `json:"artifact_reference,omitempty"`
	ResultStatus                  string `json:"result_status,omitempty"`
	ResultDigest                  string `json:"result_digest,omitempty"`
	CancellationRequested         bool   `json:"cancellation_requested"`
	StepStatus                    string `json:"step_status,omitempty"`
	Role                          string `json:"role,omitempty"`
	Profile                       string `json:"profile,omitempty"`
	SessionID                     string `json:"session_id,omitempty"`
}

type ProgressEvent struct {
	Schema        int    `json:"schema_version"`
	Sequence      uint64 `json:"sequence"`
	StateRevision int    `json:"state_revision"`
	Kind          string `json:"kind"`
	ObservedAt    string `json:"observed_at"`
	ProgressView
}

type ProgressHistory struct {
	Oldest uint64          `json:"oldest_sequence"`
	Latest uint64          `json:"latest_sequence"`
	Events []ProgressEvent `json:"events"`
}

func (h *ProgressHistory) clone() *ProgressHistory {
	if h == nil {
		return nil
	}
	out := *h
	out.Events = append([]ProgressEvent(nil), h.Events...)
	return &out
}

func displayText(text string) string {
	runes := []rune(text)
	if len(runes) > 512 {
		return string(runes[:512]) + "…"
	}
	return text
}

// ProgressSnapshot is a presentation projection, not an execution decision.
func (s *State) ProgressSnapshot() ProgressView {
	v := ProgressView{EpisodeID: s.RequestID, Stage: s.Stage, Reason: displayText(s.StageReason),
		Terminal: s.Stage.Terminal(), ResultStatus: s.ResultStatus, ResultDigest: s.ResultDigest,
		CancellationRequested: s.CancellationRequested, NextAction: "supervisor owns execution; keep observing"}
	if s.Episode != nil {
		v.Phase = s.Episode.Phase
		v.PlanDigest = s.Episode.PlanDigest
		if document := s.Episode.ImportedPlan; document != nil {
			v.ImportedPlanSHA256 = document.SHA256
		}
		if s.Episode.PlanAuthorized {
			v.PlanAcceptanceSource = displayText(s.Episode.AuthorizedBy)
		}
		if s.ProgramBinding != nil {
			v.ProgramID = s.ProgramBinding.Definition.ProgramID
			v.ProgramCardID = s.ProgramBinding.CardID
			v.ProgramDigest = s.ProgramBinding.ProgramDigest
			v.ProgramCardDigest = s.ProgramBinding.CardDigest
			for i := len(s.ProgramAcceptances) - 1; i >= 0; i-- {
				if s.ProgramAcceptances[i].PlanDigest == s.Episode.PlanDigest {
					v.ProgramAcceptancePlanDigest = s.ProgramAcceptances[i].PlanDigest
					v.ProgramAcceptanceAuthorityRef = s.ProgramAcceptances[i].AuthorityRef
					v.ProgramAcceptanceAt = s.ProgramAcceptances[i].AcceptedAt
					break
				}
			}
		}
		v.WaitingForOperator = v.Phase == string(PhasePlanAccept) && !s.Episode.PlanAuthorized && !s.CancellationRequested && !v.Terminal
		for i := len(s.Episode.Execution.Steps) - 1; i >= 0; i-- {
			step := s.Episode.Execution.Steps[i]
			if step.Phase != v.Phase {
				continue
			}
			v.StepStatus = displayText(step.Status)
			v.Role = displayText(step.Role)
			v.Profile = displayText(step.Profile)
			v.SessionID = displayText(step.Session)
			if !v.Terminal && !s.CancellationRequested {
				v.Reason = displayText(step.Reason)
			}
			if eff, ok := s.EffectByID(step.EffectID); ok && eff.Status == EffectBlocked && step.Status == StepDispatched && !v.Terminal && !s.CancellationRequested {
				v.Reason = displayText(eff.LastError)
				v.RequiresAction = true
				v.ActionKind = "inspect-reviewer"
				v.NextAction = "inspect the blocked delivery; do not resend an unknown prompt"
			}
			break
		}
	}
	if v.PlanDigest != "" {
		v.ArtifactReference = "episode artifact --episode-id " + s.RequestID + " --digest " + v.PlanDigest
	}
	if v.WaitingForOperator {
		v.RequiresAction = true
		v.ActionKind = "accept-plan"
		v.NextAction = "local operator: inspect retained plan, approve its exact digest through episode accept-plan; do not self-approve"
	}
	if v.Terminal {
		v.ActionKind = "inspect-result"
		v.NextAction = "read durable result; notification alone is not PASS"
	}
	if s.RuntimeBlock != nil {
		v.RequiresAction = true
		v.Reason = displayText(s.RuntimeBlock.Reason)
		v.ActionKind = "inspect-reviewer"
		v.NextAction = "local operator: inspect the runtime failure and retained result; submit a fresh authorized episode when resolved"
		if s.RuntimeBlock.Kind == "cleanup-unresolved" {
			v.ActionKind = "reconcile-cleanup"
			v.NextAction = "local operator: reconcile the recorded owned effects; do not start a replacement while ownership is unresolved"
		} else if v.Terminal {
			v.ActionKind = "inspect-result"
			v.NextAction = "read durable result and retained runtime diagnosis; notification alone is not PASS"
		}
	}
	return v
}

func (s *State) recordProgress() error {
	if s.Episode == nil {
		return nil
	}
	v := s.ProgressSnapshot()
	if s.Progress == nil {
		s.Progress = &ProgressHistory{Events: []ProgressEvent{}}
	}
	h := s.Progress
	if len(h.Events) > 0 && h.Events[len(h.Events)-1].ProgressView == v {
		return nil
	}
	if h.Latest == ^uint64(0) {
		return fmt.Errorf("progress sequence exhausted")
	}
	kind := "progress"
	if h.Latest == 0 {
		kind = "admitted"
	}
	if v.RequiresAction {
		kind = "operation-blocked"
	}
	if v.WaitingForOperator {
		kind = "decision-required"
	}
	if v.Terminal {
		kind = "terminal"
	}
	if v.CancellationRequested && !v.Terminal {
		kind = "cancellation-requested"
	}
	h.Latest++
	h.Events = append(h.Events, ProgressEvent{Schema: 1, Sequence: h.Latest, StateRevision: s.Revision, Kind: kind, ObservedAt: s.UpdatedAt, ProgressView: v})
	if len(h.Events) > MaxProgressUpdates {
		h.Events = append([]ProgressEvent(nil), h.Events[len(h.Events)-MaxProgressUpdates:]...)
	}
	h.Oldest = h.Events[0].Sequence
	return nil
}

func (s *State) validateProgress() error {
	if s.Progress == nil {
		return nil
	}
	h := s.Progress
	if s.Episode == nil || len(h.Events) == 0 || len(h.Events) > MaxProgressUpdates || h.Latest != h.Events[len(h.Events)-1].Sequence || h.Oldest != h.Events[0].Sequence {
		return fmt.Errorf("invalid episode progress history")
	}
	var previous uint64
	for i, e := range h.Events {
		if e.Schema != 1 || e.EpisodeID != s.RequestID || e.Sequence == 0 || (i > 0 && e.Sequence != previous+1) || e.StateRevision > s.Revision {
			return fmt.Errorf("invalid episode progress ordering")
		}
		b, _ := json.Marshal(e)
		if len(b) > 16<<10 {
			return fmt.Errorf("progress metadata exceeds 16 KiB")
		}
		previous = e.Sequence
	}
	return nil
}

// ProgressReplay reads snapshot and watermark from one committed state. A gap
// is explicit; callers may fetch a new snapshot rather than pretend full replay.
func (s *State) ProgressReplay(after uint64) ([]ProgressEvent, bool, error) {
	if s.Episode == nil {
		return nil, false, fmt.Errorf("request is not an episode")
	}
	if s.Progress == nil {
		if after != 0 {
			return nil, false, fmt.Errorf("future progress cursor")
		}
		return nil, false, nil
	}
	if after > s.Progress.Latest {
		return nil, false, fmt.Errorf("future progress cursor")
	}
	if len(s.Progress.Events) > 0 && after < s.Progress.Events[0].Sequence-1 {
		return nil, true, nil
	}
	var events []ProgressEvent
	for _, e := range s.Progress.Events {
		if e.Sequence > after {
			events = append(events, e)
		}
	}
	return events, false, nil
}
