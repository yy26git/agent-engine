package control

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/emjin/agent-engine/contract"
)

const (
	episodeHandoverSchema   = 1
	maxEpisodeHandoverBytes = 1 << 20
)

// EpisodeHandover is a bounded, content-addressed continuity record. It
// carries evidence identities and decisions, not execution authority. In
// particular it never contains launch argv or environment values.
type EpisodeHandover struct {
	SchemaVersion          int    `json:"schema_version"`
	ContentSHA256          string `json:"content_sha256"`
	EpisodeID              string `json:"episode_id"`
	RequestID              string `json:"request_id"`
	CreatedAt              string `json:"created_at"`
	SourceStateRevision    int    `json:"source_state_revision"`
	CommittedStateRevision int    `json:"committed_state_revision"`
	PreviousDigest         string `json:"previous_handover_sha256,omitempty"`

	Objective   string                          `json:"objective"`
	Obligations []HandoverObligation            `json:"obligations"`
	Source      HandoverSource                  `json:"source"`
	Episode     HandoverEpisode                 `json:"episode"`
	Profiles    []HandoverProfile               `json:"pinned_profiles,omitempty"`
	Bindings    []HandoverBinding               `json:"role_session_bindings,omitempty"`
	Steps       HandoverWindow[HandoverStep]    `json:"steps"`
	Reviews     HandoverWindow[HandoverReview]  `json:"reviews"`
	Checks      HandoverWindow[HandoverCheck]   `json:"checks"`
	Failures    HandoverWindow[HandoverFailure] `json:"failures"`
	NextAction  string                          `json:"next_action"`
	Authority   string                          `json:"authority"`
}

type HandoverWindow[T any] struct {
	Records        []T `json:"records"`
	OmittedEarlier int `json:"omitted_earlier"`
}

type HandoverObligation struct {
	ID               string   `json:"id"`
	Criterion        string   `json:"criterion"`
	RequiredChecks   []string `json:"required_checks,omitempty"`
	Disposition      string   `json:"disposition"`
	EvidenceRefs     []string `json:"evidence_refs,omitempty"`
	OriginGeneration int      `json:"origin_generation,omitempty"`
}

type HandoverSource struct {
	Workspace             string            `json:"workspace"`
	ExpectedHead          string            `json:"expected_head,omitempty"`
	RepairBranch          string            `json:"repair_branch,omitempty"`
	RepairBaseHead        string            `json:"repair_base_head,omitempty"`
	RepairBundleSHA256    string            `json:"repair_bundle_sha256,omitempty"`
	AdmittedRequestSHA256 string            `json:"admitted_request_sha256"`
	EpisodeRequestSHA256  string            `json:"episode_request_sha256,omitempty"`
	PolicySHA256          string            `json:"policy_sha256"`
	InitialBaselineSHA256 string            `json:"initial_baseline_sha256,omitempty"`
	MeasuredSnapshot      *HandoverSnapshot `json:"measured_snapshot,omitempty"`
	PlanBaseline          *HandoverSnapshot `json:"plan_baseline,omitempty"`
}

type HandoverSnapshot struct {
	SnapshotID    string `json:"snapshot_id"`
	ContentSHA256 string `json:"content_sha256"`
	Root          string `json:"root"`
	FileCount     int    `json:"file_count"`
	TakenAt       string `json:"taken_at"`
}

type HandoverEpisode struct {
	Template                string                                    `json:"template"`
	Stage                   Stage                                     `json:"stage"`
	StageReason             string                                    `json:"stage_reason,omitempty"`
	Phase                   string                                    `json:"phase"`
	Attempt                 int                                       `json:"attempt"`
	PhaseHistory            HandoverWindow[EpisodePhaseMove]          `json:"phase_history"`
	ReportDigest            string                                    `json:"report_sha256,omitempty"`
	ReportProducer          string                                    `json:"report_producer,omitempty"`
	ReportModel             string                                    `json:"report_model,omitempty"`
	PlanDigest              string                                    `json:"plan_sha256,omitempty"`
	ImportedPlanSHA256      string                                    `json:"imported_plan_sha256,omitempty"`
	PlanVersion             int                                       `json:"plan_version,omitempty"`
	PlanAuthorized          bool                                      `json:"plan_authorized"`
	AuthorizedBy            string                                    `json:"authorized_by,omitempty"`
	ProgramID               string                                    `json:"program_id,omitempty"`
	ProgramDigest           string                                    `json:"program_sha256,omitempty"`
	ProgramCardID           string                                    `json:"program_card_id,omitempty"`
	ProgramCardDigest       string                                    `json:"program_card_sha256,omitempty"`
	ProgramAuthorityRef     string                                    `json:"program_authority_ref,omitempty"`
	ProgramPolicyExpiresAt  string                                    `json:"program_policy_expires_at,omitempty"`
	ProgramAcceptance       *ProgramPlanAcceptanceReceipt             `json:"program_acceptance,omitempty"`
	PlanInvalidatedReason   string                                    `json:"plan_invalidated_reason,omitempty"`
	ResultDigest            string                                    `json:"result_sha256,omitempty"`
	ResultStatus            string                                    `json:"result_status,omitempty"`
	StartedAt               string                                    `json:"started_at"`
	Budget                  HandoverBudget                            `json:"root_budget"`
	CancellationRequested   bool                                      `json:"cancellation_requested,omitempty"`
	RuntimeBlock            string                                    `json:"runtime_block,omitempty"`
	ActiveReview            *HandoverActiveReview                     `json:"active_review,omitempty"`
	PendingStop             *HandoverStop                             `json:"pending_stop,omitempty"`
	Findings                HandoverWindow[HandoverFinding]           `json:"findings"`
	ReinvestigationBaseline *HandoverSnapshot                         `json:"reinvestigation_baseline,omitempty"`
	SourceInvalidations     HandoverWindow[EpisodeSourceInvalidation] `json:"source_invalidations"`
}

type HandoverStop struct {
	Phase  string `json:"phase"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

type HandoverActiveReview struct {
	Kind             string `json:"kind"`
	ReviewID         string `json:"review_id"`
	ExpectedReviewer string `json:"expected_reviewer,omitempty"`
	ExpectedModel    string `json:"expected_model,omitempty"`
	Generation       int    `json:"generation"`
	SnapshotID       string `json:"snapshot_id,omitempty"`
	RequestedAt      string `json:"requested_at"`
	Deadline         string `json:"deadline,omitempty"`
}

type HandoverBudget struct {
	Total     HandoverBudgetTotal   `json:"total"`
	Spent     EpisodeBudgetConsumed `json:"spent"`
	Remaining map[string]int        `json:"remaining"`
}

type HandoverBudgetTotal struct {
	Reports          int `json:"reports"`
	Plans            int `json:"plans"`
	Repairs          int `json:"repairs"`
	CompletionAudits int `json:"completion_audits"`
	WallSeconds      int `json:"wall_seconds"`
}

// HandoverProfile deliberately mirrors only non-secret profile identity. The
// digest is over this safe projection, never the full ProfileRef.
type HandoverProfile struct {
	IdentitySHA256           string        `json:"identity_sha256"`
	Name                     string        `json:"name"`
	Role                     ProfileRole   `json:"role"`
	Transport                Transport     `json:"transport"`
	Tool                     string        `json:"tool,omitempty"`
	Provider                 string        `json:"provider,omitempty"`
	Model                    string        `json:"model,omitempty"`
	Effort                   string        `json:"effort,omitempty"`
	Permission               string        `json:"permission,omitempty"`
	AccountRegion            AccountRegion `json:"account_region,omitempty"`
	Sandbox                  bool          `json:"sandbox,omitempty"`
	Worktree                 bool          `json:"worktree,omitempty"`
	ReadOnly                 bool          `json:"read_only,omitempty"`
	Confinement              string        `json:"confinement,omitempty"`
	ConfinementReceiptSHA256 string        `json:"confinement_receipt_sha256,omitempty"`
	ConfinementProven        bool          `json:"confinement_proven"`
}

type HandoverBinding struct {
	Role       ProfileRole `json:"role"`
	Backend    string      `json:"backend,omitempty"`
	SessionID  string      `json:"session_id,omitempty"`
	Label      string      `json:"label,omitempty"`
	Epoch      int         `json:"epoch,omitempty"`
	Generation int         `json:"generation,omitempty"`
	Resolved   bool        `json:"resolved"`
}

type HandoverStep struct {
	Key            string `json:"key"`
	Phase          string `json:"phase"`
	Round          int    `json:"round"`
	Role           string `json:"role,omitempty"`
	Profile        string `json:"profile,omitempty"`
	Session        string `json:"session,omitempty"`
	EffectID       string `json:"effect_id,omitempty"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	At             string `json:"at"`
	ArtifactDigest string `json:"artifact_sha256,omitempty"`
	EvidenceDigest string `json:"evidence_sha256,omitempty"`
	TurnID         string `json:"turn_id,omitempty"`
	SourceModel    string `json:"source_model,omitempty"`
	ModelEcho      string `json:"model_echo,omitempty"`
	Generation     int    `json:"generation"`
}

type HandoverReview struct {
	Phase           string `json:"phase"`
	Round           int    `json:"round"`
	Kind            string `json:"kind"`
	ReviewID        string `json:"review_id"`
	ReviewerSession string `json:"reviewer_session,omitempty"`
	OpenedAt        string `json:"opened_at"`
	Settled         bool   `json:"settled"`
	Outcome         string `json:"outcome,omitempty"`
	SettledAt       string `json:"settled_at,omitempty"`
	ModelEcho       string `json:"model_echo,omitempty"`
}

type HandoverCheck struct {
	JobID         string `json:"job_id"`
	CheckID       string `json:"check_id"`
	Status        string `json:"status"`
	ReceiptID     string `json:"receipt_id,omitempty"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
	ConfigSHA256  string `json:"config_sha256,omitempty"`
	SourceSHA256  string `json:"source_sha256,omitempty"`
	PlanSHA256    string `json:"plan_sha256,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	FinishedAt    string `json:"finished_at,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	Reason        string `json:"reason,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
}

type HandoverFailure struct {
	CycleID      string `json:"cycle_id"`
	CheckID      string `json:"check_id"`
	ObligationID string `json:"obligation_id,omitempty"`
	Signature    string `json:"signature"`
	ObservedAt   string `json:"observed_at"`
}

type HandoverFinding struct {
	PlanSHA256   string   `json:"plan_sha256"`
	StepID       string   `json:"step_id"`
	Iteration    int      `json:"iteration"`
	Kind         string   `json:"kind"`
	Path         string   `json:"path,omitempty"`
	Detail       string   `json:"detail"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	ObservedAt   string   `json:"observed_at"`
}

// CreateEpisodeHandover snapshots the exact currently committed state, retains
// it by digest, and commits the pointer only if the state revision has not
// changed. An orphan artifact after a conflict is harmless and cannot be
// mistaken for the current handover because no state points to it.
func (s *Store) CreateEpisodeHandover(requestID string) (EpisodeHandover, string, error) {
	var empty EpisodeHandover
	state, err := s.State(requestID)
	if err != nil {
		return empty, "", err
	}
	if state.Episode == nil {
		return empty, "", fmt.Errorf("agent-engine: request %q has no engineering episode", requestID)
	}
	if state.EpisodeResumeValidation != nil && state.EpisodeResumeValidation.Blocked {
		return empty, "", fmt.Errorf("agent-engine: the episode has unresolved source drift; a continuity handover cannot clear or rebase it")
	}
	releaseWorkspace, err := s.AcquireWorkspaceLease(state.Workspace, requestID)
	if err != nil {
		return empty, "", fmt.Errorf("agent-engine: cannot measure a handover while the workspace is owned: %w", err)
	}
	defer releaseWorkspace()
	state, err = s.State(requestID)
	if err != nil {
		return empty, "", err
	}
	if state.Episode == nil {
		return empty, "", fmt.Errorf("agent-engine: request %q no longer has an engineering episode", requestID)
	}
	if state.EpisodeResumeValidation != nil && state.EpisodeResumeValidation.Blocked {
		return empty, "", fmt.Errorf("agent-engine: the episode has unresolved source drift; a continuity handover cannot clear or rebase it")
	}
	measured, err := measureStableEpisodeSource(state.Workspace, s.Config().StoreRoot, s.now())
	if err != nil {
		return empty, "", err
	}

	if state.EpisodeHandoverDigest != "" {
		body, readErr := s.ReadEpisodeArtifact(requestID, state.EpisodeHandoverDigest)
		if readErr != nil {
			return empty, "", fmt.Errorf("agent-engine: latest episode handover pointer is unreadable: %w", readErr)
		}
		var prior EpisodeHandover
		if err := json.Unmarshal(body, &prior); err != nil || prior.EpisodeID != state.Episode.EpisodeID {
			return empty, "", fmt.Errorf("agent-engine: latest episode handover pointer is corrupt")
		}
		if prior.SchemaVersion != episodeHandoverSchema {
			return empty, "", fmt.Errorf("agent-engine: latest episode handover schema is unsupported")
		}
		if unsigned, err := json.Marshal(episodeHandoverUnsigned(prior)); err != nil || sha256Hex(unsigned) != prior.ContentSHA256 {
			return empty, "", fmt.Errorf("agent-engine: latest episode handover content digest is invalid")
		}
		if prior.CommittedStateRevision != prior.SourceStateRevision+1 {
			return empty, "", fmt.Errorf("agent-engine: latest episode handover revision binding is invalid")
		}
		if prior.CommittedStateRevision == state.Revision {
			if prior.Source.MeasuredSnapshot != nil && prior.Source.MeasuredSnapshot.ContentSHA256 == measured.ContentDigest {
				return prior, state.EpisodeHandoverDigest, nil
			}
			if prior.Source.MeasuredSnapshot != nil {
				if !episodeHandoverSourceRebaseAuthorized(state, prior, measured.ContentDigest) {
					return empty, "", fmt.Errorf("agent-engine: workspace source changed since the last handover; continuity export cannot bless new bytes")
				}
			}
		}
		if prior.Source.MeasuredSnapshot != nil && prior.Source.MeasuredSnapshot.ContentSHA256 != measured.ContentDigest {
			if !episodeHandoverSourceRebaseAuthorized(state, prior, measured.ContentDigest) {
				return empty, "", fmt.Errorf("agent-engine: workspace source changed since the last handover; continuity export cannot bless new bytes")
			}
		}
	}

	request, err := s.Request(requestID)
	if err != nil {
		return empty, "", err
	}
	if request.Workspace.Path != state.Workspace {
		return empty, "", fmt.Errorf("agent-engine: admitted request identity does not match episode state")
	}
	created := s.now().UTC().Format(time.RFC3339Nano)
	h := buildEpisodeHandover(state, request, created, measured)
	h.SourceStateRevision = state.Revision
	h.CommittedStateRevision = state.Revision + 1
	h.PreviousDigest = state.EpisodeHandoverDigest
	unsigned, err := json.Marshal(episodeHandoverUnsigned(h))
	if err != nil {
		return empty, "", err
	}
	h.ContentSHA256 = sha256Hex(unsigned)
	body, err := json.Marshal(h)
	if err != nil {
		return empty, "", err
	}
	if len(body) > maxEpisodeHandoverBytes {
		return empty, "", fmt.Errorf("agent-engine: handover is %d bytes, exceeding the %d-byte handover bound; large evidence must remain referenced by digest", len(body), maxEpisodeHandoverBytes)
	}
	digest := sha256Hex(body)
	if err := (&Engine{store: s, now: s.now}).retainEpisodeArtifact(requestID, digest, h); err != nil {
		return empty, "", fmt.Errorf("agent-engine: retaining episode handover: %w", err)
	}
	_, err = s.UpdateCommitted(requestID, func(current *State) error {
		if current.Revision != state.Revision || current.Episode == nil || current.Episode.EpisodeID != state.Episode.EpisodeID {
			return fmt.Errorf("agent-engine: episode changed while handover was being retained; retry from current state")
		}
		current.EpisodeHandoverDigest = digest
		return nil
	})
	if err != nil {
		return empty, "", err
	}
	return h, digest, nil
}

func episodeHandoverSourceRebaseAuthorized(state *State, prior EpisodeHandover, measuredSHA256 string) bool {
	if state == nil || state.Episode == nil || prior.Source.MeasuredSnapshot == nil || state.EpisodeHandoverDigest == "" {
		return false
	}
	for i := len(state.Episode.SourceInvalidations) - 1; i >= 0; i-- {
		event := state.Episode.SourceInvalidations[i]
		if event.PreviousHandoverSHA256 == state.EpisodeHandoverDigest &&
			event.NewSourceSHA256 == measuredSHA256 &&
			event.PreviousSourceSHA256 == prior.Source.MeasuredSnapshot.ContentSHA256 {
			return true
		}
	}
	return false
}

// ReadEpisodeHandover reads the state-pinned latest handover, verifies both its
// content address and its episode identity, and never changes episode state.
func (s *Store) ReadEpisodeHandover(requestID string) (EpisodeHandover, []byte, error) {
	var empty EpisodeHandover
	state, err := s.State(requestID)
	if err != nil {
		return empty, nil, err
	}
	if state.Episode == nil || state.EpisodeHandoverDigest == "" {
		return empty, nil, fmt.Errorf("agent-engine: episode has no retained handover; create one with episode handover")
	}
	body, err := s.ReadEpisodeArtifact(requestID, state.EpisodeHandoverDigest)
	if err != nil {
		return empty, nil, err
	}
	return s.decodeEpisodeHandover(state.Episode.EpisodeID, state.EpisodeHandoverDigest, body)
}

// ReadEpisodeHandoverDigest reads one explicitly selected handover retained
// under this request and validates the immutable file digest and episode ID.
func (s *Store) ReadEpisodeHandoverDigest(requestID, digest string) (EpisodeHandover, []byte, error) {
	var empty EpisodeHandover
	state, err := s.State(requestID)
	if err != nil {
		return empty, nil, err
	}
	if state.Episode == nil {
		return empty, nil, fmt.Errorf("agent-engine: request %q has no engineering episode", requestID)
	}
	body, err := s.ReadEpisodeArtifact(requestID, digest)
	if err != nil {
		return empty, nil, err
	}
	return s.decodeEpisodeHandover(state.Episode.EpisodeID, digest, body)
}

func (s *Store) decodeEpisodeHandover(episodeID, digest string, body []byte) (EpisodeHandover, []byte, error) {
	var empty EpisodeHandover
	var h EpisodeHandover
	if err := json.Unmarshal(body, &h); err != nil {
		return empty, nil, err
	}
	unsigned, err := json.Marshal(episodeHandoverUnsigned(h))
	if err != nil {
		return empty, nil, err
	}
	if h.SchemaVersion != episodeHandoverSchema || sha256Hex(unsigned) != h.ContentSHA256 || h.EpisodeID != episodeID || len(digest) != 64 || h.CommittedStateRevision != h.SourceStateRevision+1 {
		return empty, nil, fmt.Errorf("agent-engine: retained episode handover failed identity or digest verification")
	}
	for _, profile := range h.Profiles {
		identity := profile.IdentitySHA256
		profile.IdentitySHA256 = ""
		projection, err := json.Marshal(profile)
		if err != nil || sha256Hex(projection) != identity {
			return empty, nil, fmt.Errorf("agent-engine: retained episode handover profile identity is invalid")
		}
	}
	return h, body, nil
}

func buildEpisodeHandover(st *State, req *contract.Request, created string, measured Snapshot) EpisodeHandover {
	episode := st.Episode
	obligations := make([]HandoverObligation, 0, len(st.Obligations))
	for _, obligation := range st.Obligations {
		obligations = append(obligations, HandoverObligation{ID: obligation.ID, Criterion: obligation.Criterion, RequiredChecks: append([]string(nil), obligation.RequiredChecks...), Disposition: obligation.Disposition, EvidenceRefs: append([]string(nil), obligation.EvidenceRefs...), OriginGeneration: obligation.OriginGeneration})
	}
	profiles := make([]HandoverProfile, 0, len(st.ExecutionProfiles))
	for role, profile := range st.ExecutionProfiles {
		profiles = append(profiles, safeHandoverProfile(role, profile))
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Role < profiles[j].Role })
	bindings := make([]HandoverBinding, 0, len(st.BackendBindings))
	seenBinding := make(map[ProfileRole]bool, len(st.BackendBindings))
	for role, binding := range st.BackendBindings {
		bindings = append(bindings, HandoverBinding{Role: role, Backend: binding.Backend, SessionID: binding.SessionID, Label: binding.Label, Epoch: binding.Epoch, Generation: binding.Generation, Resolved: binding.Resolved})
		seenBinding[role] = true
	}
	for role, session := range map[ProfileRole]string{RoleWorker: st.WorkerSession, RoleReviewer: st.AuditorSession} {
		if session != "" && !seenBinding[role] {
			bindings = append(bindings, HandoverBinding{Role: role, SessionID: session, Generation: st.Generation, Resolved: true})
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Role < bindings[j].Role })
	steps := make([]HandoverStep, 0, len(episode.Execution.Steps))
	for _, step := range episode.Execution.Steps {
		steps = append(steps, HandoverStep{Key: step.Key, Phase: step.Phase, Round: step.Round, Role: step.Role, Profile: step.Profile, Session: step.Session, EffectID: step.EffectID, Status: step.Status, Reason: step.Reason, At: step.At, ArtifactDigest: step.ArtifactDigest, EvidenceDigest: step.ArtifactEvidenceDigest, TurnID: step.TurnID, SourceModel: step.SourceModel, ModelEcho: step.ModelEcho, Generation: step.Generation})
	}
	reviews := make([]HandoverReview, 0, len(episode.Execution.Reviews))
	for _, review := range episode.Execution.Reviews {
		reviews = append(reviews, HandoverReview{Phase: review.Phase, Round: review.Round, Kind: review.Kind, ReviewID: review.ReviewID, ReviewerSession: review.ReviewerSession, OpenedAt: review.OpenedAt, Settled: review.Settled, Outcome: review.Outcome, SettledAt: review.SettledAt, ModelEcho: review.ModelEcho})
	}
	checks := make([]HandoverCheck, 0, len(st.Jobs)+len(episode.Execution.Conformance)+8)
	for _, job := range st.Jobs {
		checks = append(checks, HandoverCheck{JobID: job.JobID, CheckID: job.CheckID, Status: job.Status, ReceiptID: job.ReceiptID, SnapshotID: job.SnapshotID, ConfigSHA256: job.ConfigDigest, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt})
	}
	if verification := episode.Execution.Verification; verification != nil {
		for _, receipt := range verification.Receipts {
			checks = append(checks, HandoverCheck{JobID: receipt.JobID, CheckID: receipt.CheckID, Status: receipt.Outcome, ReceiptID: receipt.ReceiptID, SnapshotID: receipt.SnapshotID, ConfigSHA256: receipt.ConfigDigest, StartedAt: receipt.StartedAt, FinishedAt: receipt.FinishedAt, Outcome: receipt.Outcome, Reason: receipt.Reason, ContentSHA256: receipt.ContentDigest})
		}
	}
	for _, receipt := range episode.Execution.Conformance {
		for _, check := range receipt.Checks {
			status, reason := "FAIL", "conformance check failed"
			if check.Passed {
				status, reason = "PASS", ""
			}
			if !check.Fresh {
				status += "_STALE"
				reason = "check was not fresh for the measured source"
			}
			checks = append(checks, HandoverCheck{JobID: fmt.Sprintf("%s#%d", receipt.StepID, receipt.Iteration), CheckID: check.CheckID, Status: status, SourceSHA256: receipt.SourceDigest, PlanSHA256: receipt.PlanDigest, StartedAt: receipt.ObservedAt, FinishedAt: receipt.ObservedAt, Outcome: status, Reason: reason, ContentSHA256: receipt.SourceDigest})
		}
	}
	failures := make([]HandoverFailure, 0, len(st.Failures))
	for _, failure := range st.Failures {
		failures = append(failures, HandoverFailure{CycleID: failure.CycleID, CheckID: failure.CheckID, ObligationID: failure.ObligationID, Signature: failure.Signature, ObservedAt: failure.ObservedAt})
	}
	findings := make([]HandoverFinding, 0)
	for _, receipt := range episode.Execution.Conformance {
		for _, deviation := range receipt.Deviations {
			findings = append(findings, HandoverFinding{PlanSHA256: receipt.PlanDigest, StepID: receipt.StepID, Iteration: receipt.Iteration, Kind: deviation.Kind, Path: deviation.Path, Detail: deviation.Detail, EvidenceRefs: append([]string(nil), deviation.EvidenceRefs...), ObservedAt: receipt.ObservedAt})
		}
	}
	var planBaseline *HandoverSnapshot
	if episode.PlanBaseline != nil {
		planBaseline = &HandoverSnapshot{SnapshotID: episode.PlanBaseline.SnapshotID, ContentSHA256: episode.PlanBaseline.ContentDigest, Root: episode.PlanBaseline.Root, FileCount: episode.PlanBaseline.FileCount, TakenAt: episode.PlanBaseline.TakenAt}
	}
	var runtimeBlock string
	if st.RuntimeBlock != nil {
		runtimeBlock = st.RuntimeBlock.Reason
	}
	var activeReview *HandoverActiveReview
	if review := st.ActiveReview; review != nil {
		activeReview = &HandoverActiveReview{Kind: string(review.Kind), ReviewID: review.ReviewID, ExpectedReviewer: review.ExpectedReviewer, ExpectedModel: review.ExpectedModel, Generation: review.Generation, SnapshotID: review.SnapshotID, RequestedAt: review.RequestedAt, Deadline: review.Deadline}
	}
	var pendingStop *HandoverStop
	if stop := episode.Stop; stop != nil {
		pendingStop = &HandoverStop{Phase: stop.Phase, Reason: stop.Reason, At: stop.At}
	}
	var reinvestigationBaseline *HandoverSnapshot
	if episode.ReinvestigationBaseline != nil {
		reinvestigationBaseline = &HandoverSnapshot{SnapshotID: episode.ReinvestigationBaseline.SnapshotID, ContentSHA256: episode.ReinvestigationBaseline.ContentDigest, Root: episode.ReinvestigationBaseline.Root, FileCount: episode.ReinvestigationBaseline.FileCount, TakenAt: episode.ReinvestigationBaseline.TakenAt}
	}
	var programID, programDigest, cardID, cardDigest, authorityRef, policyExpiresAt string
	var programAcceptance *ProgramPlanAcceptanceReceipt
	if binding := st.ProgramBinding; binding != nil {
		programID, programDigest, cardID, cardDigest = binding.Definition.ProgramID, binding.ProgramDigest, binding.CardID, binding.CardDigest
		authorityRef, policyExpiresAt = binding.AuthorityRef, binding.PolicyExpiresAt
		if episode.PlanAuthorized {
			for i := len(st.ProgramAcceptances) - 1; i >= 0; i-- {
				if st.ProgramAcceptances[i].PlanDigest == episode.PlanDigest {
					receipt := st.ProgramAcceptances[i]
					programAcceptance = &receipt
					break
				}
			}
		}
	}
	spent := episode.Budget.Consumed
	if start, err := time.Parse(time.RFC3339, episode.StartedAt); err == nil {
		spent.WallSeconds = max(0, int(safeHandoverElapsed(start, created)))
	}
	total := HandoverBudgetTotal{Reports: episode.Budget.ReportAttempts, Plans: episode.Budget.PlanAttempts, Repairs: episode.Budget.RepairIterations, CompletionAudits: episode.Budget.CompletionAudits, WallSeconds: episode.Budget.WallSeconds}
	remaining := map[string]int{"reports": total.Reports - spent.Reports, "plans": total.Plans - spent.Plans, "repairs": total.Repairs - spent.Repairs, "completion_audits": total.CompletionAudits - spent.CompletionAudit, "wall_seconds": total.WallSeconds - spent.WallSeconds}
	rootBudget := HandoverBudget{Total: total, Spent: spent, Remaining: remaining}
	handover := EpisodeHandover{
		SchemaVersion: episodeHandoverSchema, EpisodeID: episode.EpisodeID, RequestID: st.RequestID, CreatedAt: created,
		Objective: req.Objective, Obligations: obligations,
		Source:   HandoverSource{Workspace: req.Workspace.Path, ExpectedHead: req.Workspace.ExpectedHead, RepairBranch: st.RepairBranch, RepairBaseHead: st.RepairBaseHead, RepairBundleSHA256: st.RepairBundleSHA256, AdmittedRequestSHA256: st.RequestDigest, EpisodeRequestSHA256: st.EpisodeRequestSHA256, PolicySHA256: st.PolicyDigest, InitialBaselineSHA256: st.BaselineDigest, MeasuredSnapshot: handoverSnapshot(measured), PlanBaseline: planBaseline},
		Episode:  HandoverEpisode{Template: st.EpisodeTemplate, Stage: st.Stage, StageReason: st.StageReason, Phase: episode.Phase, Attempt: episode.Attempt, PhaseHistory: handoverWindow(episode.PhaseHistory, len(episode.PhaseHistory)), ReportDigest: episode.ReportDigest, ReportProducer: episode.ReportProducer, ReportModel: episode.ReportModel, PlanDigest: episode.PlanDigest, PlanVersion: episode.PlanVersion, PlanAuthorized: episode.PlanAuthorized, AuthorizedBy: episode.AuthorizedBy, ProgramID: programID, ProgramDigest: programDigest, ProgramCardID: cardID, ProgramCardDigest: cardDigest, ProgramAuthorityRef: authorityRef, ProgramPolicyExpiresAt: policyExpiresAt, ProgramAcceptance: programAcceptance, PlanInvalidatedReason: episode.InvalidatedReason, ResultDigest: st.ResultDigest, ResultStatus: st.ResultStatus, StartedAt: episode.StartedAt, Budget: rootBudget, CancellationRequested: st.CancellationRequested, RuntimeBlock: runtimeBlock, ActiveReview: activeReview, PendingStop: pendingStop, Findings: handoverWindow(findings, len(findings)), ReinvestigationBaseline: reinvestigationBaseline, SourceInvalidations: handoverWindow(episode.SourceInvalidations, MaxEpisodeSourceInvalidations)},
		Profiles: profiles, Bindings: bindings,
		Steps: handoverWindow(steps, len(steps)), Reviews: handoverWindow(reviews, len(reviews)), Checks: handoverWindow(checks, len(checks)), Failures: handoverWindow(failures, len(failures)),
		NextAction: episodeHandoverNextAction(st),
		Authority:  "This artifact records continuity only. Resume this same episode by default with its existing pinned roles and bindings. It does not authorize worker replacement, profile rebinding, plan acceptance, workspace release, or any new write. Replacement requires a separate explicit local authorization and verified release of the prior writer.",
	}
	if episode.ImportedPlan != nil {
		handover.Episode.ImportedPlanSHA256 = episode.ImportedPlan.SHA256
	}
	return handover
}

func handoverSnapshot(snapshot Snapshot) *HandoverSnapshot {
	return &HandoverSnapshot{
		SnapshotID: snapshot.SnapshotID, ContentSHA256: snapshot.ContentDigest,
		Root: snapshot.Root, FileCount: snapshot.FileCount, TakenAt: snapshot.TakenAt,
	}
}

func safeHandoverProfile(role ProfileRole, ref ProfileRef) HandoverProfile {
	profile := HandoverProfile{Name: ref.Name, Role: role, Transport: ref.Transport, Tool: ref.Tool, Provider: ref.Provider, Model: ref.Model, Effort: ref.Effort, Permission: ref.Permission, AccountRegion: ref.AccountRegion, Sandbox: ref.Sandbox, Worktree: ref.Worktree, ReadOnly: ref.ReadOnly, Confinement: ref.Confinement, ConfinementReceiptSHA256: ref.ConfinementReceiptSHA256, ConfinementProven: ref.Proven}
	identity, _ := json.Marshal(profile)
	profile.IdentitySHA256 = sha256Hex(identity)
	return profile
}

func episodeHandoverNextAction(st *State) string {
	if st.CancellationRequested {
		return "Reconcile cancellation and prove all owned writers are released before any resume; this artifact does not start or replace a worker."
	}
	if st.Episode.Stop != nil {
		return "Finish publishing the already-committed episode stop result; do not restart the rejected phase or dispatch another worker step."
	}
	if st.Stage.Terminal() {
		return "Inspect the terminal result and its evidence; this episode requires no further worker action."
	}
	if st.Episode.Phase == string(contract.PhasePlanAccept) && st.Episode.PlanDigest != "" && !st.Episode.PlanAuthorized {
		return fmt.Sprintf("A local operator must review and accept or reject exact plan digest %s; this handover does not accept it.", st.Episode.PlanDigest)
	}
	if st.RuntimeBlock != nil {
		return "Resolve the recorded runtime block, then resume this same episode only after ownership recovery proves no duplicate writer can run."
	}
	if st.Episode.ReinvestigationBaseline != nil {
		return fmt.Sprintf("Continue read-only re-investigation from source snapshot %s: produce a new report, obtain its independent review, draft a new plan, then require local acceptance of that exact plan before any implementation.", st.Episode.ReinvestigationBaseline.SnapshotID)
	}
	if st.ActiveReview != nil {
		return fmt.Sprintf("Reconcile open %s review %q with its already-bound reviewer session %q; do not open a duplicate review or substitute a reviewer.", st.ActiveReview.Kind, st.ActiveReview.ReviewID, st.ActiveReview.ExpectedReviewer)
	}
	return fmt.Sprintf("Resume the existing episode %q at phase %s after ownership recovery; keep its already-pinned profiles and role/session bindings. Do not create a replacement worker from this handover.", st.Episode.EpisodeID, st.Episode.Phase)
}

func handoverWindow[T any](values []T, max int) HandoverWindow[T] {
	if len(values) <= max {
		return HandoverWindow[T]{Records: append([]T{}, values...)}
	}
	return HandoverWindow[T]{Records: append([]T(nil), values[len(values)-max:]...), OmittedEarlier: len(values) - max}
}

func safeHandoverElapsed(start time.Time, created string) int64 {
	now, err := time.Parse(time.RFC3339Nano, created)
	if err != nil || now.Before(start) {
		return 0
	}
	return int64(now.Sub(start).Seconds())
}

// episodeHandoverUnsigned removes the digest field to avoid a self-referential
// hash while keeping the on-disk artifact fully typed and versioned.
func episodeHandoverUnsigned(h EpisodeHandover) any {
	h.ContentSHA256 = ""
	return h
}
