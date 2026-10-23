package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/emjin/agent-engine/contract"
)

// The request state machine.
//
// This file declares the types and the legal-transition table. It does not yet
// enforce them: EC1 establishes what a request is made of, and EC2 owns the
// engine that applies events. Declaring the table here, rather than leaving it
// implicit in whichever consumer happens to need it, is what stops the policy
// from growing a second, incompatible state machine in a later package.
//
// The governing idea is that one machine describes the request and nothing else
// does. Agent activity, a successful send, a green command and a paragraph of
// an auditor's prose are all inputs to this machine, and none of them is
// allowed to change it directly.

// Stage is the request's execution phase.
type Stage string

const (
	// StageReady means the request is admitted and durable but no effects have
	// started. A prerequisite that is merely not established yet leaves the
	// request here with a reason; only a proven unsupported capability makes a
	// request terminally blocked.
	StageReady Stage = "READY"
	// StageWorking means an agent is executing under an active grant.
	StageWorking Stage = "WORKING"
	// StageCheckpointReview means a review is owed and mutation grants are
	// suspended.
	StageCheckpointReview Stage = "CHECKPOINT_REVIEW"
	// StageDiagnostic means an independent investigation of a stuck cycle is
	// owed. It approves nothing.
	StageDiagnostic Stage = "DIAGNOSTIC"
	// StageRefreshing means the implementer is being replaced and the request
	// is held until a new generation is committed.
	StageRefreshing Stage = "REFRESHING"
	// StageVerifying means required checks are running against a frozen
	// snapshot.
	StageVerifying Stage = "VERIFYING"
	// StageCompletionAudit means an independent completion review is owed. Only
	// a PASS for the active request here can verify the request.
	StageCompletionAudit Stage = "COMPLETION_AUDIT"
	// StageWaitingExternal means the request is blocked on something outside
	// itself, with a saved resume stage and an attributable reason.
	StageWaitingExternal Stage = "WAITING_EXTERNAL"
	// StageVerified is terminal: obligations met, checks satisfied, independent
	// completion audit passed.
	StageVerified Stage = "VERIFIED"
	// StageFailed is terminal: evidence disproved an obligation after permitted
	// recovery was exhausted.
	StageFailed Stage = "FAILED"
	// StageBlocked is terminal: required evidence, access, identity or safe
	// recovery could not be established.
	StageBlocked Stage = "BLOCKED"
	// StageCancelled is terminal: cancellation was requested and its effects
	// reconciled.
	StageCancelled      Stage = "CANCELLED"
	StageReportComplete Stage = "REPORT_COMPLETE"
)

// Terminal reports whether a stage is final.
func (s Stage) Terminal() bool {
	switch s {
	case StageVerified, StageFailed, StageBlocked, StageCancelled, StageReportComplete:
		return true
	}
	return false
}

// Valid reports whether the stage is one this build knows.
func (s Stage) Valid() bool {
	switch s {
	case StageReady, StageWorking, StageCheckpointReview, StageDiagnostic,
		StageRefreshing, StageVerifying, StageCompletionAudit, StageWaitingExternal,
		StageVerified, StageFailed, StageBlocked, StageCancelled, StageReportComplete:
		return true
	}
	return false
}

// ReviewKind discriminates the three review modes.
//
// The discriminator is not decoration. CHECKPOINT and DIAGNOSTIC reviews judge
// whether to continue and what to do next; only COMPLETION can approve. Keeping
// the modes in one enum with an explicit allowed-outcome set is what makes
// "a diagnostic is not an approval" structural instead of a rule someone has to
// remember.
type ReviewKind string

const (
	ReviewCheckpoint ReviewKind = "CHECKPOINT"
	ReviewDiagnostic ReviewKind = "DIAGNOSTIC"
	ReviewCompletion ReviewKind = "COMPLETION"
)

// ReviewOutcome is a reviewer's answer.
type ReviewOutcome string

const (
	OutcomeContinue        ReviewOutcome = "CONTINUE"
	OutcomeRedirect        ReviewOutcome = "REDIRECT"
	OutcomeCollectEvidence ReviewOutcome = "COLLECT_EVIDENCE"
	OutcomeDiagnose        ReviewOutcome = "DIAGNOSE"
	OutcomeRefresh         ReviewOutcome = "REFRESH"
	OutcomePass            ReviewOutcome = "PASS"
	OutcomeFail            ReviewOutcome = "FAIL"
	OutcomeBlocked         ReviewOutcome = "BLOCKED"
	OutcomeGuidance        ReviewOutcome = "GUIDANCE"
)

// allowedOutcomes is the per-mode outcome allowlist.
//
// A reviewer answering outside its mode's set is refused rather than coerced.
// The alternative is a schema that accepts whatever arrives and a gate that
// decides afterwards whether it likes it, which is how a diagnostic response
// ends up recorded as an approval.
var allowedOutcomes = map[ReviewKind]map[ReviewOutcome]bool{
	ReviewCheckpoint: {
		OutcomeContinue: true, OutcomeRedirect: true, OutcomeCollectEvidence: true,
		OutcomeDiagnose: true, OutcomeRefresh: true, OutcomeBlocked: true,
	},
	ReviewDiagnostic: {
		OutcomeGuidance: true, OutcomeRefresh: true, OutcomeBlocked: true,
	},
	ReviewCompletion: {
		OutcomePass: true, OutcomeFail: true, OutcomeBlocked: true,
	},
}

// Allows reports whether a kind may answer with an outcome.
func (k ReviewKind) Allows(o ReviewOutcome) bool { return allowedOutcomes[k][o] }

// Approves reports whether this kind can approve the request at all.
//
// CHECKPOINT and DIAGNOSTIC cannot, by construction. This is the single
// property that stops a stuck-worker consultation from being mistaken for
// sign-off that the work is done.
func (k ReviewKind) Approves() bool { return k == ReviewCompletion }

// ActiveReview is the review currently owed, if any.
//
// Every field is identity rather than content, because the point is to refuse a
// response that belongs to a different request, generation, snapshot or review.
// A late answer to a superseded review is exactly the failure mode that produces
// a confident approval of the wrong work.
type ActiveReview struct {
	// Kind is the review mode.
	Kind ReviewKind `json:"kind"`
	// ReviewID is this review's own identity.
	ReviewID string `json:"review_id"`
	// ExpectedReviewer is the full session ID that must answer.
	ExpectedReviewer string `json:"expected_reviewer,omitempty"`
	// ExpectedModel is the admission-pinned selection. Missing answer model
	// metadata is allowed; an observed conflicting model is refused.
	//
	// It is recorded from the PINNED profile at the moment the review is opened,
	// so a reviewer swapped to a different model afterwards cannot answer under
	// the old one. Empty means the pinned profile named no model, which is a
	// statement about the configuration rather than a licence to invent one.
	ExpectedModel string `json:"expected_model,omitempty"`
	// Generation is the implementer generation the review is scoped to.
	Generation int `json:"generation"`
	// SnapshotID pins the content the reviewer must judge.
	SnapshotID string `json:"snapshot_id,omitempty"`
	// RequestedAt and Deadline bound how long the review may remain owed.
	RequestedAt string `json:"requested_at"`
	Deadline    string `json:"deadline,omitempty"`
	// AnswerNonce correlates an answer to this specific request for a judgement.
	//
	// It exists because a review ID alone is not enough once an answer can arrive
	// out of band: a session that was asked twice, or that replays an old
	// response, would otherwise be answering a review that has since been closed
	// and reopened. The nonce is issued here and must be echoed in the answer.
	AnswerNonce string `json:"answer_nonce,omitempty"`
}

// NewActiveReview opens a review with a correlation nonce already issued.
//
// Every review goes through here. The alternative — constructing the struct at
// each call site — is how a review ends up open without a nonce, and a review
// without a nonce cannot be proven to have received the answer that approved it.
// Deriving the nonce rather than generating it randomly also means reconstructing
// a review after a restart produces the same value.
func NewActiveReview(kind ReviewKind, reviewID, expectedReviewer string, generation, attempt int) *ActiveReview {
	return &ActiveReview{
		Kind:             kind,
		ReviewID:         reviewID,
		ExpectedReviewer: expectedReviewer,
		Generation:       generation,
		AnswerNonce:      reviewNonce(reviewID, generation, attempt),
	}
}

// SessionBinding is a backend-qualified session identity for one role.
type SessionBinding struct {
	// Backend is the transport that minted this session. It is part of the
	// identity, not metadata: the same ID from two backends is two sessions.
	Backend string `json:"backend"`
	// SessionID is the backend's own identity.
	SessionID string `json:"session_id"`
	// Label is the deterministic activation label, when the backend supports one.
	Label string `json:"label,omitempty"`
	// Epoch and Generation pin when this binding was made, so a stale binding can
	// be recognised rather than used.
	Epoch      int `json:"epoch,omitempty"`
	Generation int `json:"generation,omitempty"`
	// ProcessPID is the OS process that owns this session, when the backend owns
	// one of its own.
	//
	// It is recorded so a later run can prove that process is gone before it
	// creates anything. A session id cannot answer "is the thing that made this
	// still running?", and guessing either way is expensive: assume-dead creates
	// a second worker on one workspace, assume-alive strands the first.
	ProcessPID int `json:"process_pid,omitempty"`
	// ProcessStart is the owning process's start token.
	//
	// It is recorded because a pid alone does not identify a process across
	// time: on a long-lived machine the number is reused. Comparing the token is
	// what turns "some process has this pid" into "the original worker is still
	// running" — and lets a reused pid be recognised as the original being gone.
	ProcessStart string `json:"process_start,omitempty"`
	// Resolved records whether the backend confirmed this identity. A binding that
	// was never confirmed is a claim, and claiming a session is how a phantom
	// worker enters the routing table.
	Resolved bool `json:"resolved"`
}

// BackendFor returns the session reference a role is bound to.
//
// A pre-v3 state has only bare IDs, so it resolves to the legacy Agent Deck
// binding the migration created. That fallback is a statement about HISTORY: every
// such state was produced through Agent Deck, and an unqualified ID cannot be
// routed safely.
func (s *State) BackendFor(role ProfileRole) (SessionBinding, bool) {
	if b, ok := s.BackendBindings[role]; ok && b.Backend != "" && b.SessionID != "" {
		return b, true
	}
	var legacy string
	switch role {
	case RoleWorker:
		legacy = s.WorkerSession
	case RoleReviewer:
		legacy = s.AuditorSession
	case RoleCheckpoint:
		legacy = s.CheckpointSession
	}
	if legacy == "" {
		return SessionBinding{}, false
	}
	return SessionBinding{Backend: LegacyBackendAgentDeck, SessionID: legacy, Resolved: true}, true
}

// BackendForID finds the backend that owns a session id.
//
// Effect routing needs this because an effect records a session, and that id is
// only unique inside its backend. Matching by id alone would route a stale
// effect to whatever transport happens to hold the same string.
func (s *State) BackendForID(sessionID string) (SessionBinding, bool) {
	if sessionID == "" {
		return SessionBinding{}, false
	}
	var matched SessionBinding
	found := false
	for _, b := range s.BackendBindings {
		if b.SessionID != sessionID {
			continue
		}
		if found && matched.Backend != b.Backend {
			return SessionBinding{}, false
		}
		matched = b
		found = true
	}
	if found {
		return matched, true
	}
	for role, id := range map[ProfileRole]string{
		RoleWorker: s.WorkerSession, RoleReviewer: s.AuditorSession, RoleCheckpoint: s.CheckpointSession,
	} {
		if id == sessionID && id != "" {
			return SessionBinding{Backend: LegacyBackendAgentDeck, SessionID: id, Resolved: true}, true
		}
		_ = role
	}
	return SessionBinding{}, false
}

// RuntimePin is the approved runtime identity recorded on a request.
type RuntimePin struct {
	// SourceCommit is the reviewed upstream commit.
	SourceCommit string `json:"source_commit,omitempty"`
	// BinarySHA256 is the measured digest of the executable.
	BinarySHA256 string `json:"binary_sha256,omitempty"`
	// VerifiedAt is when the pin was measured for this request.
	VerifiedAt string `json:"verified_at,omitempty"`
	// LockFile records which lock manifest approved it.
	LockFile string `json:"lock_file,omitempty"`
}

// Pinned reports whether a runtime identity was actually recorded.
func (r RuntimePin) Pinned() bool {
	return strings.TrimSpace(r.SourceCommit) != "" && strings.TrimSpace(r.BinarySHA256) != ""
}

// CompletionEvidence is the durable record of what was actually established
// before an approval was accepted.
//
// It is one record rather than a scattering of fields because the property that
// matters is the conjunction: this exact content, checked by this exact receipt
// set, judged by this exact reviewer, for this exact generation. Reading any one
// of those separately cannot tell you whether the approval still describes the
// work it was granted for.
type CompletionEvidence struct {
	// SnapshotID and ContentDigest pin the content that was verified.
	SnapshotID    string `json:"snapshot_id"`
	ContentDigest string `json:"content_sha256"`
	// BaselineDigest and FinalDigest are the measured source identities either
	// side of the work, so code_changed is a measurement rather than a guess
	// derived from how many attempts it took.
	BaselineDigest string `json:"baseline_sha256,omitempty"`
	FinalDigest    string `json:"final_sha256,omitempty"`
	// ReceiptIDs are the managed check receipts this approval rests on.
	ReceiptIDs []string `json:"receipt_ids,omitempty"`
	// ReviewID, ReviewerSession and Generation identify the judgement.
	ReviewID        string `json:"review_id"`
	ReviewerSession string `json:"reviewer_session"`
	Generation      int    `json:"generation"`
	// ReportDigest is the measured digest of the committed reasoning body.
	ReportDigest string `json:"report_sha256"`
	ApprovedAt   string `json:"approved_at"`
	// CheckCount is how many required checks actually executed.
	CheckCount int `json:"check_count"`
}

// Obligation is one tracked unit of required work.
type Obligation struct {
	ID             string   `json:"id"`
	Criterion      string   `json:"criterion"`
	RequiredChecks []string `json:"required_checks,omitempty"`
	// Disposition is OPEN, ADVANCED, FAILED or UNKNOWN. It is never set from a
	// checkpoint's mere arrival; only a reviewed evidence-backed judgement moves
	// it.
	Disposition string `json:"disposition"`
	// EvidenceRefs names the immutable evidence backing the disposition.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	// OriginGeneration records the generation that raised the obligation, which
	// stays meaningful after a refresh.
	OriginGeneration int `json:"origin_generation,omitempty"`
}

// Obligation dispositions.
const (
	DispositionOpen     = "OPEN"
	DispositionAdvanced = "ADVANCED"
	DispositionFailed   = "FAILED"
	DispositionUnknown  = "UNKNOWN"
)

// CheckJob is one managed verification run.
type CheckJob struct {
	// JobID is assigned when the job starts and is never reused.
	JobID string `json:"job_id"`
	// CheckID names the operator registry entry.
	CheckID string `json:"check_id"`
	// SnapshotID is the immutable content the job reads.
	SnapshotID string `json:"snapshot_id,omitempty"`
	// ConfigDigest pins the resolved check configuration.
	ConfigDigest string `json:"config_digest,omitempty"`
	// Status is PENDING, RUNNING, PASS, FAIL or BLOCKED.
	Status string `json:"status"`
	// ReceiptID references the immutable receipt, when one exists.
	ReceiptID string `json:"receipt_id,omitempty"`
	// StartedAt and FinishedAt bound the job.
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// Check job statuses.
const (
	JobPending = "PENDING"
	JobRunning = "RUNNING"
	JobPass    = "PASS"
	JobFail    = "FAIL"
	JobBlocked = "BLOCKED"
)

// Activation is a durable intent to create or start a session.
//
// It is a record rather than a closure, because after a restart there is no
// decision context left to re-run — only the durable statement of what was owed
// and to whom. Its Status is the record of how far that intent got, and an
// intent found in a non-terminal status is ambiguous: it may or may not have
// taken effect, and it must be reconciled rather than replayed.
type Activation struct {
	// ActivationID is derived from the decision that created it.
	ActivationID string `json:"activation_id"`
	// Role is "implementer" or "auditor".
	Role string `json:"role"`
	// SessionID is the confirmed full session ID, once known.
	SessionID string `json:"session_id,omitempty"`
	// Label is the unique label used to find a session whose creation outcome is
	// unknown. Ambiguous creation is reconciled by label and profile inventory,
	// never by creating another session and hoping.
	Label string `json:"label,omitempty"`
	// Status is PENDING, DISPATCHING, CONFIRMED or BLOCKED.
	Status string `json:"status"`
	// Revision is the state revision the intent was created at.
	Revision  int    `json:"revision"`
	CreatedAt string `json:"created_at"`
	// LastError explains a blocked activation.
	LastError string `json:"last_error,omitempty"`
}

// Activation statuses.
const (
	ActivationPending     = "PENDING"
	ActivationDispatching = "DISPATCHING"
	ActivationConfirmed   = "CONFIRMED"
	ActivationBlocked     = "BLOCKED"
)

// Unresolved reports whether an activation's outcome is still unknown.
func (a Activation) Unresolved() bool {
	return a.Status == ActivationPending || a.Status == ActivationDispatching
}

// MaxActivations bounds the intent ledger.
const MaxActivations = 16

// HistoryEntry is one applied transition.
type HistoryEntry struct {
	At       string `json:"at"`
	From     Stage  `json:"from"`
	To       Stage  `json:"to"`
	Event    string `json:"event"`
	Note     string `json:"note,omitempty"`
	Revision int    `json:"revision"`
}

// UnresolvedActivations returns the session intents whose outcome is still
// unknown.
//
// These are the entries that must never be dropped to make room: an intent in
// this set may or may not have created a session, and that ambiguity is the only
// record of it.
func (s *State) UnresolvedActivations() []Activation {
	var out []Activation
	for _, a := range s.Activations {
		if a.Unresolved() {
			out = append(out, a)
		}
	}
	return out
}

// ErrActivationLedgerFull is returned when a new session intent is requested
// while unresolved intent already fills the ledger.
var ErrActivationLedgerFull = errors.New("agent-engine: session intent ledger is full of unresolved intent")

// RecordActivation adds a session intent, refusing to evict existing work.
//
// The bound is a backpressure signal, not a garbage collector. A request whose
// ledger is full is a request that has already failed to confirm several
// sessions; dropping one of those records to make room would hide exactly the
// fact an operator needs.
func (s *State) RecordActivation(a Activation) error {
	for _, existing := range s.Activations {
		if existing.ActivationID == a.ActivationID {
			if existing.Label != a.Label || existing.Role != a.Role {
				return fmt.Errorf("activation identity collision")
			}
			return nil
		}
	}
	if len(s.UnresolvedActivations()) >= MaxActivations {
		return fmt.Errorf("%w: %d unresolved intents; reconcile them before starting another session",
			ErrActivationLedgerFull, MaxActivations)
	}
	s.Activations = append(s.Activations, a)
	if len(s.Activations) > MaxActivations {
		// Trim only resolved entries, oldest first. They need no recovery, so
		// losing them costs nothing.
		kept := make([]Activation, 0, MaxActivations)
		drop := len(s.Activations) - MaxActivations
		for _, existing := range s.Activations {
			if drop > 0 && !existing.Unresolved() {
				drop--
				continue
			}
			kept = append(kept, existing)
		}
		s.Activations = kept
	}
	return nil
}

// MaxHistory bounds the transition log.
const MaxHistory = 200

// State is the sole mutable authority for one request.
type State struct {
	// SchemaVersion guards the on-disk format.
	SchemaVersion int `json:"schema_version"`
	// Revision increments on every accepted write. Every event revalidates it
	// inside the transaction, so a slow decision cannot overwrite a newer one it
	// never observed.
	Revision int              `json:"revision"`
	Progress *ProgressHistory `json:"progress_updates,omitempty"`
	// Supervisor records the last process that owned this request and how it
	// left. An active record left behind after the ownership lock becomes free
	// identifies an abrupt process death; a committed stop intent distinguishes
	// an intentional service shutdown from that case.
	Supervisor *SupervisorLifecycle `json:"supervisor,omitempty"`

	// RequestID and RequestDigest identify the admitted request. The digest is
	// what makes duplicate submission a no-op and conflicting submission a
	// refusal.
	RequestID     string `json:"request_id"`
	RequestDigest string `json:"request_sha256"`
	// PolicyDigest pins the operator configuration in force at admission. A
	// changed configuration does not silently apply to an in-flight request; it
	// requires explicit reconciliation or a new request.
	PolicyDigest string `json:"policy_digest"`
	// LocalPilot is frozen at admission; legacy states remain ungranted.
	LocalPilot           *LocalPilotGrant `json:"local_pilot,omitempty"`
	ExecutionAssurance   string           `json:"execution_assurance,omitempty"`
	EpisodeRequestSHA256 string           `json:"episode_request_sha256,omitempty"`
	RepairBranch         string           `json:"repair_branch,omitempty"`
	RepairBaseHead       string           `json:"repair_base_head,omitempty"`
	RepairGitCommonDir   string           `json:"repair_git_common_dir,omitempty"`
	// RepairBundleTargets is the locally admitted Git identity map for a
	// multi-repository workspace bundle. It is copied from a verified retained
	// preparation receipt and checked before every sensitive phase transition.
	RepairBundleSHA256  string                      `json:"repair_bundle_sha256,omitempty"`
	RepairBundleTargets []RepairBundleTargetReceipt `json:"repair_bundle_targets,omitempty"`
	// TaskID is the stable identity that survives a generation change, so a
	// replacement implementer is provably continuing the same task.
	TaskID string `json:"task_id,omitempty"`

	Stage Stage `json:"stage"`
	// StageReason explains a nonterminal stage in one bounded line.
	StageReason string `json:"stage_reason,omitempty"`

	// Workspace is the canonical absolute root.
	Workspace string `json:"workspace"`
	// Profile scopes session resolution.
	Profile string `json:"profile,omitempty"`

	// CancellationRequested records intent before any effect is attempted.
	CancellationRequested   bool   `json:"cancellation_requested,omitempty"`
	CancellationRequestedAt string `json:"cancellation_requested_at,omitempty"`
	CancellationReason      string `json:"cancellation_reason,omitempty"`

	// Obligations is the minimum open-obligation ledger.
	Obligations []Obligation `json:"obligations"`
	// Checks is the required check set, by registry ID.
	Checks []string `json:"checks,omitempty"`
	// Jobs are the managed verification jobs.
	Jobs []CheckJob `json:"jobs,omitempty"`
	// ActiveReview is the review currently owed.
	ActiveReview *ActiveReview   `json:"active_review,omitempty"`
	RuntimeBlock *RuntimeBlock   `json:"runtime_block,omitempty"`
	Cleanup      *CleanupAttempt `json:"cleanup_attempt,omitempty"`

	// WorkerSession and AuditorSession are full session IDs. Titles are display
	// metadata and are never used for routing, because a title can be reused and
	// a full ID cannot.
	WorkerSession     string `json:"worker_session,omitempty"`
	AuditorSession    string `json:"auditor_session,omitempty"`
	CheckpointSession string `json:"checkpoint_session,omitempty"`
	DiagnosticSession string `json:"diagnostic_session,omitempty"`
	// ImplementationSessions is every session that has held the implementer role,
	// including retired ones.
	//
	// The current worker alone is not enough to refuse self-approval: a refresh
	// replaces the worker, and the session that did the work is the one whose
	// judgement must not approve it. Keeping the whole lineage means a replaced
	// implementer can never come back as its own reviewer.
	ImplementationSessions []string `json:"implementation_sessions,omitempty"`
	// Generation counts implementer replacements. It increments only when a
	// validated handoff and a distinct session have been committed, so a
	// generation number always corresponds to a real starting point.
	Generation int `json:"generation"`
	// BindingBoundary is the cursor from which the current generation's events
	// are read, so a retired worker's late output is refused rather than
	// mistaken for the new one's.
	BindingBoundary uint64 `json:"binding_boundary,omitempty"`

	// RepairCycles, AuditAttempts, DiagnosticRounds and FailedChecks are
	// lifetime counters, distinct from activity counts. They are what bound
	// speculation across a restart.
	RepairCycles     int      `json:"repair_cycles"`
	AuditAttempts    int      `json:"audit_attempts"`
	DiagnosticRounds int      `json:"diagnostic_rounds"`
	FailedCheckKeys  []string `json:"failed_check_keys,omitempty"`

	// Limits is the effective ceiling admitted with this request.
	Limits Limits `json:"limits"`

	// Activations is the durable session-intent ledger.
	Activations []Activation `json:"activations,omitempty"`

	// Replacement is the durable record of a generation change, if one is in
	// progress. Its phase is what distinguishes a crash before the binding from
	// a crash after it, which have different correct recoveries.
	Replacement Replacement `json:"replacement,omitempty"`
	// HandoffGeneration is the generation the current handoff describes.
	HandoffGeneration int `json:"handoff_generation,omitempty"`

	// Failures is the objective failure ledger: check identity plus a normalized
	// error, with the original text preserved beside it.
	//
	// It is counted from objective evidence rather than from the worker's
	// self-declared hypothesis, so renaming a hypothesis cannot buy another
	// attempt.
	Failures []FailureRecord `json:"failures,omitempty"`

	// Effects is the durable instruction ledger: what this control layer owes a
	// session, and how far each got.
	//
	// It is the reason an instruction survives a crash. A decision that only
	// lived in memory is lost when the process dies, and the workflow then
	// believes it already told the worker something it never said.
	Effects []Effect `json:"effects,omitempty"`

	// Questions and Decisions are the durable memory indexes. Both are
	// task-scoped and survive a generation change, so a replacement implementer
	// reads accepted decisions instead of re-deriving them.
	//
	// The identity counters are separate fields rather than derived from the list
	// lengths, so compaction can never recycle an identifier.
	Questions       []QuestionRecord `json:"questions,omitempty"`
	Decisions       []DecisionRecord `json:"decisions,omitempty"`
	NextQuestionSeq int              `json:"next_question_seq,omitempty"`
	NextDecisionSeq int              `json:"next_decision_seq,omitempty"`
	// DecisionAuthorityRole is the only role permitted to ACCEPT a decision.
	// Empty means none may, which is the safe direction.
	DecisionAuthorityRole string `json:"decision_authority_role,omitempty"`

	// ReportDigest references the committed report backing the current stage.
	ReportDigest string `json:"report_digest,omitempty"`
	// ResultDigest references the committed terminal result.
	ResultDigest string `json:"result_digest,omitempty"`
	// ResultStatus is the terminal outcome once committed.
	ResultStatus string `json:"result_status,omitempty"`
	// Completion is the evidence an approval rests on. It is set in the same
	// transaction that commits the terminal result, so a committed PASS always
	// has this record beside it and a missing record can never be read as an
	// approval that simply did not need any.
	Completion *CompletionEvidence `json:"completion,omitempty"`
	// PendingResult holds a prewritten result body digest between the body write
	// and the state commit.
	//
	// This is what closes the crash window. The body is durable before the
	// approval commits, and the approval carries its digest, so a crash between
	// the two leaves a recoverable pointer rather than a closed review with
	// nothing behind it.
	PendingResult string `json:"pending_result_sha256,omitempty"`
	// BaselineDigest is the content identity measured when the request started
	// working, so the final comparison is against a recorded starting point.
	BaselineDigest string `json:"baseline_sha256,omitempty"`

	// BackendBindings are the backend-qualified session identities, keyed by
	// role. They are the authority for routing an effect: a bare session ID is
	// only unique inside the backend that minted it.
	//
	// WorkerSession and AuditorSession are RETAINED as the legacy fields, because
	// a v2 state carries only those and an older binary must be able to read it.
	BackendBindings map[ProfileRole]SessionBinding `json:"backend_bindings,omitempty"`
	// Episode is the durable identity, phase and pinned-artifact record of a
	// multi-phase engineering episode.
	//
	// It is separate from a child's supervision Stage because a request may be
	// WORKING during planning and again during repair, so "which step are we
	// on" is not answerable from Stage alone after a crash.
	Episode *EpisodeRoot `json:"episode,omitempty"`
	// EpisodeWorkerRelease is a local operator's exact-binding request to stop
	// the current worker. It never grants authority to create a replacement.
	EpisodeWorkerRelease *EpisodeWorkerRelease `json:"episode_worker_release,omitempty"`
	// ProgramBinding pins the locally authorised delegated-program definition
	// and the one card this episode represents. It is installed atomically with
	// admission and is never supplied by an episode artifact.
	ProgramBinding *EpisodeProgramBinding `json:"program_binding,omitempty"`
	// ProgramAcceptances is the append-only evidence for exact local policy
	// decisions at PLAN_ACCEPT. It never substitutes for PlanAuthorized.
	ProgramAcceptances []ProgramPlanAcceptanceReceipt `json:"program_acceptances,omitempty"`
	// EpisodeHandoverDigest points to the most recently committed immutable
	// handover artifact. Older handovers remain addressable through the
	// previous-digest chain in each artifact.
	EpisodeHandoverDigest string `json:"episode_handover_sha256,omitempty"`
	// EpisodeResumeValidation is the latest source measurement performed by a
	// new supervisor after owned-process quiescence. A blocked drift record is
	// sticky: a later measurement or handover cannot silently bless different
	// source bytes as a replacement baseline.
	EpisodeResumeValidation *EpisodeResumeValidation `json:"episode_resume_validation,omitempty"`
	// EpisodeTemplate is the LOCALLY approved template this episode admitted
	// under. It is recorded so a resumed run cannot pick a different shape.
	EpisodeTemplate string `json:"episode_template,omitempty"`

	// OwnedProcesses are the OS processes this supervisor started, recorded at
	// startup rather than at activation.
	//
	// A pid first recorded when a session binding is committed leaves a crash
	// window — process up, binding not yet written — in which an orphan exists
	// that no record can name. Recording the intent first is what makes the next
	// start able to reap it.
	OwnedProcesses map[string]OwnedProcess `json:"owned_processes,omitempty"`

	// PendingGuidance are instructions owed to the worker at a safe turn boundary.
	//
	// They are durable because a coding agent will not take a second prompt while
	// a turn is live, and an instruction that was never queued is an instruction
	// that is never received.
	PendingGuidance []Guidance `json:"pending_guidance,omitempty"`

	// PendingAssessments are durable completion claims awaiting an independent
	// judgement.
	//
	// A claim is retained rather than dropped when another review is already
	// open, and promoted when that review settles. Without this, a worker that
	// stops while a checkpoint is in flight would have its only statement that
	// the work finished discarded — and nothing would ever ask for the audit.
	PendingAssessments []AssessmentCandidate `json:"pending_assessments,omitempty"`

	// ConsumedJournalSeq is the local journal position a committed decision has
	// already accounted for.
	//
	// It advances only when the decision is durable, so a crash between appending
	// an observation and committing its decision replays rather than loses.
	ConsumedJournalSeq uint64 `json:"consumed_journal_seq,omitempty"`
	// IngestedPositions are the per-backend observation positions already ingested.
	//
	// They are keyed by BACKEND and are never compared across backends: an Agent
	// Deck cursor and an ACP connection ordinal are different units, and merging
	// them produces a bug that presents as a race.
	IngestedPositions map[string]uint64 `json:"ingested_positions,omitempty"`
	// JournalPath records where this request's observation journal lives.
	JournalPath string `json:"journal_path,omitempty"`

	// PinnedProfiles records the profile identity each role was launched under.
	//
	// The identity is pinned rather than the name alone, so an operator editing
	// what "worker" means cannot retroactively change how an in-flight request
	// runs.
	PinnedProfiles []ProfileRef `json:"pinned_profiles,omitempty"`
	// ExecutionProfiles pins every task role at admission, before any launch.
	ExecutionProfiles map[ProfileRole]ProfileRef `json:"execution_profiles,omitempty"`
	// WorkflowDefinitionSHA and WorkflowInputDigest identify the admitted graph and
	// this run's input. They are pinned at admission so a run's identity cannot
	// be re-pointed at different instructions.
	WorkflowDefinitionSHA string `json:"workflow_definition_sha256,omitempty"`
	WorkflowInputDigest   string `json:"workflow_input_sha256,omitempty"`
	// WorkflowChildren is the immutable child map: task ID to derived request ID.
	// It is written once at admission and never recomputed, so a child cannot
	// appear, disappear or be re-pointed.
	WorkflowChildren   map[string]string `json:"workflow_children,omitempty"`
	WorkflowChildOrder []string          `json:"workflow_child_order,omitempty"`
	// LaunchedAt is when the current epoch's scheduler was started.
	//
	// It is committed as part of the launch intent, so a run whose scheduler start
	// is unknown is visible as "an epoch was launched at T" rather than as nothing
	// having happened.
	LaunchedAt string `json:"launched_at,omitempty"`

	// LaunchEpoch is the current outer-launch epoch of a root.
	//
	// It exists because a result from an earlier launch may not be presented as
	// current: the same admitted children are re-executed on a new launch, and a
	// stale result would otherwise satisfy a dependency it no longer describes.
	LaunchEpoch int `json:"launch_epoch,omitempty"`

	// WorkspaceLeaseRoot and WorkspaceLeaseEpoch record the root's ownership of a
	// workspace for one run epoch. Children are delegated within it.
	WorkspaceLeaseRoot  string `json:"workspace_lease_root,omitempty"`
	WorkspaceLeaseEpoch int    `json:"workspace_lease_epoch,omitempty"`
	// Transfers records every ownership transfer this request has been through.
	Transfers []TransferRecord `json:"transfers,omitempty"`
	// RetiredOwners names the legacy owners that have been stood down.
	//
	// It is kept so a second transfer cannot quietly re-admit an owner that was
	// already retired, and so an operator can see what used to be able to write
	// this workspace.
	RetiredOwners []string `json:"retired_owners,omitempty"`
	// ChildCapability is the unforgeable authorisation to activate this root's
	// children. It is minted by the engine and is the only thing that may admit a
	// nested request; a caller's declared origin never is.
	ChildCapability ExecutionCapability `json:"child_capability"`

	// RuntimePin records WHICH approved runtime artifact this request was admitted
	// against.
	//
	// Checking the pin at launch is not enough on its own: it proves the executable
	// was reviewed at the moment it ran, but leaves no record of what the run was
	// admitted under. A request whose provenance does not name its runtime cannot
	// be audited against the artifact it actually used.
	RuntimePin RuntimePin `json:"runtime_pin"`

	// LegacyAssurance is the assurance level of preserved legacy approvals.
	//
	// It is "unverified" and stays that way. The legacy engine recorded that it
	// decided work was correct without ever establishing who decided or what was
	// checked, so its approvals are readable history and nothing more.
	LegacyAssurance string `json:"legacy_assurance,omitempty"`

	// BusCursor is the last consumed event position, when a runtime is attached.
	BusCursor uint64 `json:"bus_cursor,omitempty"`
	// CursorSet distinguishes "never consumed" from "consumed up to zero".
	CursorSet bool `json:"cursor_set,omitempty"`

	// Supervision is the engine's own clock bookkeeping: when it last ticked, when
	// it last woke a model and why, and any bounded external wait currently
	// pausing active debt.
	Supervision SupervisionState `json:"supervision"`

	// Checkpoint is the obligation-review bookkeeping.
	//
	// It is kept as one nested object rather than loose fields because the
	// interesting relationships are internal to it: debt only accumulates while
	// grants are live, a pending review suppresses duplicate wakes, and only a
	// reviewed advance resets the debt. Splitting it across the state would make
	// those invariants easy to break one field at a time.
	Checkpoint Checkpoint `json:"checkpoint"`

	// History is the bounded transition log.
	History []HistoryEntry `json:"history,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CurrentSchemaVersion is the state format this build writes.
//
// v7 adds structured report-only tasks and COMPLETE data results.
// v6 pins imported plan documents and their reviewed mappings. Older executables
// must refuse these states rather than silently drop the original plan.
// v4 adds bounded durable episode progress metadata, with no fabricated history
// when older states are read. v3 adds backend-qualified session bindings, transport metadata on effects, the
// per-source ingestion position and the locally consumed journal position. v2
// added the completion evidence record, the implementation lineage and the
// prewritten-result pointer.
//
// The migration rule is uniform across every version: a field a previous schema
// did not have is initialized UNKNOWN rather than inferred. The safe reading of
// "this state does not record who approved" is "nobody proved who approved",
// never "it must have been fine".
const CurrentSchemaVersion = 7

// StateSchemaV1 is the format written before completion evidence existed.
const StateSchemaV1 = 1

// StateSchemaV2 is the format written before backend-qualified bindings.
//
// Every migration names the version it PRODUCES rather than CurrentSchemaVersion.
// A migration that writes the current version silently satisfies the next
// migration's precondition, so the step after it never runs.
const StateSchemaV2 = 2

// Clone returns a deep copy.
//
// Every slice and pointer is copied, not just History. A shallow copy would let
// a caller that appends to a ledger, or edits a job in place, mutate the live
// state it was only reading — and the next decision would then be made from
// state nobody chose to change.
func (s *State) Clone() *State {
	if s == nil {
		return nil
	}
	out := *s
	out.Progress = s.Progress.clone()
	if s.Supervisor != nil {
		lifecycle := *s.Supervisor
		out.Supervisor = &lifecycle
	}
	if s.EpisodeResumeValidation != nil {
		validation := *s.EpisodeResumeValidation
		out.EpisodeResumeValidation = &validation
	}
	if s.EpisodeWorkerRelease != nil {
		release := *s.EpisodeWorkerRelease
		out.EpisodeWorkerRelease = &release
	}
	if s.LocalPilot != nil {
		grant := *s.LocalPilot
		out.LocalPilot = &grant
	}
	if s.Obligations != nil {
		out.Obligations = make([]Obligation, len(s.Obligations))
		for i, o := range s.Obligations {
			out.Obligations[i] = o
			if o.RequiredChecks != nil {
				out.Obligations[i].RequiredChecks = append([]string(nil), o.RequiredChecks...)
			}
			if o.EvidenceRefs != nil {
				out.Obligations[i].EvidenceRefs = append([]string(nil), o.EvidenceRefs...)
			}
		}
	}
	if s.Checks != nil {
		out.Checks = append([]string(nil), s.Checks...)
	}
	if s.Jobs != nil {
		out.Jobs = append([]CheckJob(nil), s.Jobs...)
	}
	if s.Activations != nil {
		out.Activations = append([]Activation(nil), s.Activations...)
	}
	if s.Effects != nil {
		out.Effects = append([]Effect(nil), s.Effects...)
	}
	if s.Failures != nil {
		out.Failures = append([]FailureRecord(nil), s.Failures...)
	}
	if s.Questions != nil {
		out.Questions = append([]QuestionRecord(nil), s.Questions...)
	}
	if s.Decisions != nil {
		out.Decisions = append([]DecisionRecord(nil), s.Decisions...)
		for i := range out.Decisions {
			if s.Decisions[i].EvidenceRefs != nil {
				out.Decisions[i].EvidenceRefs = append([]string(nil), s.Decisions[i].EvidenceRefs...)
			}
		}
	}
	if s.FailedCheckKeys != nil {
		out.FailedCheckKeys = append([]string(nil), s.FailedCheckKeys...)
	}
	if s.ImplementationSessions != nil {
		out.ImplementationSessions = append([]string(nil), s.ImplementationSessions...)
	}
	if s.RetiredOwners != nil {
		out.RetiredOwners = append([]string(nil), s.RetiredOwners...)
	}
	if s.PinnedProfiles != nil {
		out.PinnedProfiles = append([]ProfileRef(nil), s.PinnedProfiles...)
		for i, p := range s.PinnedProfiles {
			out.PinnedProfiles[i] = p.Profile().Ref()
		}
	}
	if s.Episode != nil {
		body, err := json.Marshal(s.Episode)
		if err != nil {
			panic(fmt.Errorf("cloning episode: %w", err))
		}
		var root EpisodeRoot
		if err := json.Unmarshal(body, &root); err != nil {
			panic(fmt.Errorf("cloning episode: %w", err))
		}
		out.Episode = &root
	}
	if s.ProgramBinding != nil {
		body, err := json.Marshal(s.ProgramBinding)
		if err != nil {
			panic(fmt.Errorf("cloning program binding: %w", err))
		}
		var binding EpisodeProgramBinding
		if err := json.Unmarshal(body, &binding); err != nil {
			panic(fmt.Errorf("cloning program binding: %w", err))
		}
		out.ProgramBinding = &binding
	}
	if s.ProgramAcceptances != nil {
		out.ProgramAcceptances = append([]ProgramPlanAcceptanceReceipt(nil), s.ProgramAcceptances...)
	}
	if s.ExecutionProfiles != nil {
		out.ExecutionProfiles = map[ProfileRole]ProfileRef{}
		for role, p := range s.ExecutionProfiles {
			out.ExecutionProfiles[role] = p.Profile().Ref()
		}
	}
	if s.WorkflowChildren != nil {
		out.WorkflowChildren = make(map[string]string, len(s.WorkflowChildren))
		for k, v := range s.WorkflowChildren {
			out.WorkflowChildren[k] = v
		}
	}
	if s.BackendBindings != nil {
		out.BackendBindings = make(map[ProfileRole]SessionBinding, len(s.BackendBindings))
		for k, v := range s.BackendBindings {
			out.BackendBindings[k] = v
		}
	}
	if s.IngestedPositions != nil {
		out.IngestedPositions = make(map[string]uint64, len(s.IngestedPositions))
		for k, v := range s.IngestedPositions {
			out.IngestedPositions[k] = v
		}
	}
	if s.WorkflowChildOrder != nil {
		out.WorkflowChildOrder = append([]string(nil), s.WorkflowChildOrder...)
	}
	if s.Transfers != nil {
		out.Transfers = make([]TransferRecord, len(s.Transfers))
		copy(out.Transfers, s.Transfers)
	}
	if s.Completion != nil {
		c := *s.Completion
		if s.Completion.ReceiptIDs != nil {
			c.ReceiptIDs = append([]string(nil), s.Completion.ReceiptIDs...)
		}
		out.Completion = &c
	}
	if s.History != nil {
		out.History = append([]HistoryEntry(nil), s.History...)
	}
	out.Checkpoint = s.Checkpoint.clone()
	out.Supervision = s.Supervision
	if s.ActiveReview != nil {
		r := *s.ActiveReview
		out.ActiveReview = &r
	}
	if s.RuntimeBlock != nil {
		b := *s.RuntimeBlock
		out.RuntimeBlock = &b
	}
	if s.Cleanup != nil {
		a := *s.Cleanup
		out.Cleanup = &a
	}
	return &out
}

// transition records a stage change in the history log.
//
// EC1 declares the machine; EC2 owns the engine that applies events. This helper
// exists so a stage change always lands in the bounded log with its revision,
// rather than each caller remembering to append one.
func (s *State) transition(to Stage, note, at string) error {
	if !to.Valid() {
		return fmt.Errorf("%w: cannot transition to unknown stage %q", ErrInvalidState, to)
	}
	from := s.Stage
	s.Stage = to
	s.StageReason = note
	s.History = append(s.History, HistoryEntry{
		At: at, From: from, To: to, Event: "transition", Note: note, Revision: s.Revision,
	})
	if len(s.History) > MaxHistory {
		s.History = s.History[len(s.History)-MaxHistory:]
	}
	return nil
}

// Transition moves the request to a new stage, recording the reason.
func (s *State) Transition(to Stage, note, at string) error { return s.transition(to, note, at) }

// Stage describes a stage for operator output.
func (s Stage) String() string { return string(s) }

// ErrInvalidState is returned when persisted state fails validation.
var ErrInvalidState = errors.New("agent-engine: persisted state is invalid")

// ErrRoleCollision is returned when one session would hold two roles that must be
// held by different sessions.
var ErrRoleCollision = errors.New("agent-engine: the implementer and the reviewer must be different sessions")

// NoteImplementation records that a session held the implementer role.
//
// It is append-only and never removes an entry, because the point is historical:
// the session that did the work is exactly the one that must not later approve it,
// and a retired worker that reappears as a reviewer is the same violation.
func (s *State) NoteImplementation(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	for _, existing := range s.ImplementationSessions {
		if existing == sessionID {
			return
		}
	}
	s.ImplementationSessions = append(s.ImplementationSessions, sessionID)
}

// IsImplementationSession reports whether a session ever held the implementer
// role for this request.
func (s *State) IsImplementationSession(sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	if sessionID == s.WorkerSession {
		return true
	}
	for _, existing := range s.ImplementationSessions {
		if existing == sessionID {
			return true
		}
	}
	return false
}

// SetBinding assigns the session roles and refuses a collision.
//
// The check is on the EFFECTIVE binding after the merge, not on the two values
// the caller happened to pass. A caller that supplies only one role is changing
// the pair, and validating the arguments rather than the result is precisely how
// a partial rebind collapses the two roles into one session.
func (s *State) SetBinding(worker, auditor string) error {
	prevWorker := s.WorkerSession
	if w := strings.TrimSpace(worker); w != "" {
		s.WorkerSession = w
	}
	if a := strings.TrimSpace(auditor); a != "" {
		s.AuditorSession = a
	}
	// The current worker joins the lineage as soon as it is bound, so a later
	// replacement cannot erase the fact that this session did the work.
	s.NoteImplementation(s.WorkerSession)

	if s.WorkerSession != "" && s.WorkerSession == s.AuditorSession {
		// Roll back rather than leave a collapsed pair behind: a refused bind
		// must not itself change the binding it refused.
		if prevWorker != "" {
			s.WorkerSession = prevWorker
		}
		return fmt.Errorf("%w: session %s cannot implement and review the same request",
			ErrRoleCollision, s.AuditorSession)
	}
	if s.AuditorSession != "" && s.IsImplementationSession(s.AuditorSession) {
		if prevWorker != "" {
			s.WorkerSession = prevWorker
		}
		return fmt.Errorf("%w: session %s previously implemented this request and cannot review it",
			ErrRoleCollision, s.AuditorSession)
	}
	return nil
}

// validate refuses state that cannot be safely continued.
func (s *State) validate() error {
	if s.Episode != nil {
		if err := contract.ValidateImportedPlanDocument(s.Episode.ImportedPlan); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidState, err)
		}
	}
	if s.RuntimeBlock != nil {
		b := s.RuntimeBlock
		switch b.Kind {
		case "review-delivery", "reviewer-unavailable", "review-invalid", "review-timeout", "episode-timeout", "cleanup-unresolved":
		default:
			return fmt.Errorf("invalid runtime block kind")
		}
		if b.Reason == "" || b.Generation < 1 || b.FirstObservedAt == "" {
			return fmt.Errorf("invalid runtime block")
		}
	}
	if s.Cleanup != nil && (s.Cleanup.Attempts < 0 || s.Cleanup.Deadline == "") {
		return fmt.Errorf("invalid cleanup attempt")
	}

	if err := s.validateProgress(); err != nil {
		return err
	}
	if !s.Stage.Valid() {
		return fmt.Errorf("%w: unknown stage %q", ErrInvalidState, s.Stage)
	}
	if strings.TrimSpace(s.RequestID) == "" {
		return fmt.Errorf("%w: request_id is empty", ErrInvalidState)
	}
	if err := contract.ValidateDigest(s.RequestDigest); err != nil {
		return fmt.Errorf("%w: request_sha256 %s", ErrInvalidState, err)
	}
	if s.Revision < 0 {
		return fmt.Errorf("%w: revision is negative", ErrInvalidState)
	}
	for name, v := range map[string]int{
		"repair_cycles":     s.RepairCycles,
		"audit_attempts":    s.AuditAttempts,
		"diagnostic_rounds": s.DiagnosticRounds,
		"generation":        s.Generation,
	} {
		if v < 0 {
			return fmt.Errorf("%w: %s is negative", ErrInvalidState, name)
		}
	}
	// A request can never hold the same session in both engineering roles. This is
	// checked on every committed write rather than only at the CLI, so no
	// migration, recovery or replay path can produce the collapsed state that
	// would make the completion audit self-certifying.
	if s.WorkerSession != "" && s.WorkerSession == s.AuditorSession {
		return fmt.Errorf("%w: session %s is bound as both implementer and reviewer",
			ErrRoleCollision, s.WorkerSession)
	}
	if s.AuditorSession != "" && s.IsImplementationSession(s.AuditorSession) {
		return fmt.Errorf("%w: reviewer %s is inside this request's implementation lineage",
			ErrRoleCollision, s.AuditorSession)
	}
	for _, id := range []string{s.CheckpointSession, s.DiagnosticSession} {
		if id != "" && s.IsImplementationSession(id) {
			return fmt.Errorf("%w: review session belongs to implementation lineage", ErrRoleCollision)
		}
	}
	// A committed result must carry a terminal status, and a stage carrying a
	// result must be terminal.
	//
	// The converse is deliberately NOT required. A request can legitimately
	// become terminal — blocked because nobody can review it, for instance — and
	// only then have its result published. Demanding the result already exist
	// would make every terminal transition unreachable until the very thing that
	// transition reports had already been committed.
	if s.ResultDigest != "" {
		if s.ResultStatus == "" {
			return fmt.Errorf("%w: a result digest is recorded with no result status", ErrInvalidState)
		}
		if !s.Stage.Terminal() {
			return fmt.Errorf("%w: stage %s is not terminal but a result is committed", ErrInvalidState, s.Stage)
		}
	}
	// A terminal PASS is the one outcome that asserts the work is correct, so it
	// is the one outcome that cannot exist without the evidence behind it. A
	// committed PASS carrying no completion record is refused rather than
	// reported, because a caller cannot act on an approval it is merely being
	// warned about.
	// A replacement paused mid-protocol is not an ordinary request: the old worker
	// may be stopped, the candidate may be unbound, and the generation boundary may
	// or may not have been captured. A build that does not know the protocol would
	// read it as a healthy WORKING request with an unexpected session, which is
	// exactly the state where a stale writer can come back.
	if s.Replacement.Phase != "" && s.Replacement.Phase != GenActive {
		if !understandsReplacementPhase(s.Replacement.Phase) {
			return fmt.Errorf("%w: this request has a replacement paused at phase %q, which this build "+
				"does not understand; finish or export it before proceeding",
				ErrUnsupportedReplacement, s.Replacement.Phase)
		}
	}
	if s.ResultStatus == string(contract.StatusPass) && s.Completion == nil {
		return fmt.Errorf("%w: a committed PASS has no completion evidence; the approval would be unbacked",
			ErrInvalidState)
	}
	return nil
}

// LegacyBackendAgentDeck is the backend every pre-v3 binding belongs to.
//
// A v2 state records bare session IDs with no transport, and every such state in
// existence was produced through Agent Deck. Mapping them is a statement about
// HISTORY rather than an inference about capability, which is why it is safe to
// apply automatically during migration.
const LegacyBackendAgentDeck = "agent-deck"
