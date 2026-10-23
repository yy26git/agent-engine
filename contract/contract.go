// Package contract defines the versioned JSON request and result types that
// every caller of agent-engine speaks.
//
// Two properties drive the whole design, and both come from the same problem:
// a request that has been admitted must be identifiable forever, by content,
// without re-reading whoever submitted it.
//
//  1. A request has a stable identity that survives a caller crash. The same
//     request_id carrying the same bytes is the same request and must never
//     produce a second set of effects. The same request_id carrying different
//     bytes is a conflict, and it must start nothing at all — silently
//     overwriting, or worse, running both, would mean the caller's intent is
//     ambiguous and the system is guessing on its behalf.
//
//  2. A result is a durable, independently readable fact. It is published after
//     the state that justifies it, and once published its identity and outcome
//     never change. A caller that restarts retrieves the same result rather than
//     asking again and possibly receiving a different answer.
package contract

// Version is the schema version this build accepts and writes.
//
// It is checked on every decode. A future version is refused rather than
// reinterpreted: reading an unknown field as absent would drop a safety
// constraint, which is the one failure this package exists to prevent.
const Version = 1

// Operation is what the caller is asking to be done.
//
// The set is deliberately small. Four operations plus an internal audit mode
// cover the real work, and every extra public operation is another nearly
// identical workflow to keep consistent.
type Operation string

const (
	// OpInvestigate produces evidence-backed findings. It never changes code.
	OpInvestigate Operation = "INVESTIGATE"
	// OpRepair changes code, and only with explicit permission.
	OpRepair Operation = "REPAIR"
	// OpVerify runs configured checks against a pinned snapshot.
	OpVerify Operation = "VERIFY"
	// OpGenerateReport synthesises supplied evidence into a report.
	OpGenerateReport Operation = "GENERATE_REPORT"
)

// validOperations is the closed set. An unknown operation is refused rather
// than defaulted, because defaulting would guess at whether code changes were
// permitted.
var validOperations = map[Operation]bool{
	OpInvestigate:    true,
	OpRepair:         true,
	OpVerify:         true,
	OpGenerateReport: true,
}

// CodeChangeAllowed reports whether this operation may change code when the
// request explicitly permits it.
//
// Only REPAIR can. INVESTIGATE, VERIFY and GENERATE_REPORT are read-only by
// construction, and admitting them with allow_code_changes=true would mean the
// caller asked for something the operation cannot honour — a silent downgrade
// is worse than a refusal, because the caller would believe it was protected.
func (o Operation) CodeChangeAllowed() bool { return o == OpRepair }

// Valid reports whether the operation is one this build understands.
func (o Operation) Valid() bool { return validOperations[o] }

// String implements fmt.Stringer.
func (o Operation) String() string { return string(o) }

// Origin records who asked. It is provenance, never authentication.
//
// The CLI is a same-user trust boundary, so this field exists to make the
// origin of a request attributable after the fact, not to grant it capability.
// Nothing may read filesystem authority out of it.
type Origin struct {
	Kind     string `json:"kind"`
	CallerID string `json:"caller_id"`
}

// Workspace identifies the code the request operates on.
type Workspace struct {
	// Path is the working directory, resolved to a canonical absolute root
	// before admission.
	Path string `json:"path"`
	// ExpectedHead pins the revision the caller believes it is starting from.
	// Empty means the caller made no claim and none is enforced.
	ExpectedHead string `json:"expected_head,omitempty"`
}

// Obligation is one thing the request must actually achieve.
//
// Obligations exist because a stage advancing is not a goal being met. Without
// an explicit list, the only available evidence is activity — commits, changed
// files, a green command — and a worker can produce plenty of that without
// having done the work.
type Obligation struct {
	ID             string   `json:"id"`
	Criterion      string   `json:"criterion"`
	RequiredChecks []string `json:"required_checks,omitempty"`
}

// EvidenceKind classifies how an evidence item was produced.
type EvidenceKind string

const (
	// EvidenceExternalRun is output from a run the caller performed.
	EvidenceExternalRun EvidenceKind = "external-run"
	// EvidenceManagedCommand is output from a check this control executed.
	EvidenceManagedCommand EvidenceKind = "managed-command"
	// EvidenceArtifact is a supplied file such as a diff or a log.
	EvidenceArtifact EvidenceKind = "artifact"
)

func (k EvidenceKind) valid() bool {
	switch k {
	case EvidenceExternalRun, EvidenceManagedCommand, EvidenceArtifact:
		return true
	}
	return false
}

// Evidence is one immutable, digest-pinned input.
type Evidence struct {
	Path   string       `json:"path"`
	SHA256 string       `json:"sha256"`
	Kind   EvidenceKind `json:"kind"`
}

// Constraints bounds what the worker may change.
//
// Read-only is a capability, not a prompt, so the default is deny. A request
// that omits this block gets no write authority whatsoever.
type Constraints struct {
	// AllowCodeChanges must be explicitly true for REPAIR. It must be absent or
	// false for every other operation.
	AllowCodeChanges bool `json:"allow_code_changes"`
	// AllowedPaths and ProtectedPaths are caller-declared scope hints, checked
	// against the operator's approved roots at admission.
	AllowedPaths   []string `json:"allowed_paths,omitempty"`
	ProtectedPaths []string `json:"protected_paths,omitempty"`
}

// VerificationItem names a check the operator registry must resolve.
//
// The caller names a check, never a command. Resolving a name to argv happens
// in operator-owned configuration, so a request cannot smuggle a shell command
// past the registry by calling it a check.
type VerificationItem struct {
	CheckID string `json:"check_id"`
}

// Limits bounds the work. Zero means the operator default applies.
type Limits struct {
	RepairCycles  int `json:"repair_cycles,omitempty"`
	WorkerGens    int `json:"worker_generations,omitempty"`
	AuditAttempts int `json:"audit_attempts,omitempty"`
}

// DefaultLimits bound speculative work when a request does not ask for more.
//
// Three is not arbitrary: three genuinely different repairs failing on one
// defect is evidence that the repair strategy itself is the problem, not the
// defect. A caller may ask for fewer. A caller may not ask for more than the
// operator's configured ceiling, which is checked at admission.
const (
	DefaultRepairCycles  = 3
	DefaultWorkerGens    = 3
	DefaultAuditAttempts = 3
)

// Effective returns the request's limits with operator defaults filled in.
func (l Limits) Effective() Limits {
	out := l
	if out.RepairCycles <= 0 {
		out.RepairCycles = DefaultRepairCycles
	}
	if out.WorkerGens <= 0 {
		out.WorkerGens = DefaultWorkerGens
	}
	if out.AuditAttempts <= 0 {
		out.AuditAttempts = DefaultAuditAttempts
	}
	return out
}

// Request is the immutable admitted unit of work.
type Request struct {
	Version      int                `json:"version"`
	RequestID    string             `json:"request_id"`
	Origin       Origin             `json:"origin"`
	Operation    Operation          `json:"operation"`
	Workspace    Workspace          `json:"workspace"`
	Objective    string             `json:"objective"`
	Obligations  []Obligation       `json:"obligations"`
	Evidence     []Evidence         `json:"evidence,omitempty"`
	Constraints  Constraints        `json:"constraints"`
	Verification []VerificationItem `json:"verification,omitempty"`
	Limits       Limits             `json:"limits,omitempty"`
}

// MaxRequestIDLen bounds the request identifier.
//
// The ID becomes a directory name, so it is validated rather than sanitised.
// Rewriting a hostile ID would let two distinct requests map onto one request
// directory, which is precisely the collision the content digest exists to
// detect.
const MaxRequestIDLen = 128

// Result is the durable, caller-facing outcome.
type Result struct {
	Version       int    `json:"version"`
	RequestID     string `json:"request_id"`
	RequestSHA256 string `json:"request_sha256"`
	TaskID        string `json:"task_id,omitempty"`
	Status        Status `json:"status"`
	// ExecutionAssurance is distinct from whether the engineering checks passed.
	ExecutionAssurance string    `json:"execution_assurance,omitempty"`
	Summary            string    `json:"summary,omitempty"`
	Findings           []Finding `json:"findings,omitempty"`
	// FinishedAt is when the request reached its terminal state.
	FinishedAt string `json:"finished_at,omitempty"`
	// CodeChanged reports whether the request modified code.
	//
	// It is reported rather than inferred from a diff, because a request that
	// changed nothing and one that changed everything both produce diffs
	// depending on where you look, and a caller deciding whether to review the
	// work needs the direct answer.
	CodeChanged bool `json:"code_changed"`
	// Verification is the required-check evidence, each tied to a receipt and the
	// snapshot it ran against.
	Verification []VerificationResult `json:"verification,omitempty"`
	// CompletionAudit is the independent review that produced this outcome.
	CompletionAudit *CompletionAuditRef `json:"completion_audit,omitempty"`
	// Workspace describes the content the outcome applies to.
	Workspace *ResultWorkspace `json:"workspace,omitempty"`
	// Artifacts are the bounded human companions, digest-pinned.
	Artifacts []ResultArtifact `json:"artifacts,omitempty"`
	// Ready distinguishes "not finished yet" from "finished with no result".
	//
	// A caller asking for a result before the request is terminal must be told
	// so explicitly. Returning an empty or partial result object would let a
	// caller mistake a nonterminal stage for a completed one, which is the
	// single most damaging way for this contract to lie.
	Ready bool `json:"ready"`
	// Stage is the current nonterminal stage when Ready is false.
	Stage string `json:"stage,omitempty"`
	// Reason explains a nonterminal stage in one bounded line.
	Reason string `json:"reason,omitempty"`
}

// VerificationResult is one required check's evidence.
type VerificationResult struct {
	CheckID string `json:"check_id"`
	// ReceiptID references the immutable receipt for this run.
	ReceiptID string `json:"receipt_id,omitempty"`
	// Outcome is PASS, FAIL or BLOCKED.
	Outcome string `json:"outcome"`
	// SnapshotID pins the content the check read, so a receipt cannot be reused
	// to approve a different set of bytes.
	SnapshotID string `json:"snapshot_id,omitempty"`
	// Reason explains a non-PASS outcome, so a blocked harness is
	// distinguishable from a failing check at the call site.
	Reason string `json:"reason,omitempty"`
	// ExitCode is the process status, or -1 when none was obtained.
	ExitCode int `json:"exit_code"`
	// ContentSHA256 pins the exact bytes this check read.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// Discovered is how many test cases the run reported executing. A required
	// test check that discovered nothing is carried here as zero rather than
	// omitted, so "ran no tests" is visible instead of merely absent.
	Discovered int `json:"discovered"`
	// StdoutSHA256 and StderrSHA256 pin the transcript without inlining it.
	StdoutSHA256 string `json:"stdout_sha256,omitempty"`
	StderrSHA256 string `json:"stderr_sha256,omitempty"`
}

// CompletionAuditRef identifies the independent review behind an outcome.
type CompletionAuditRef struct {
	ReviewID string `json:"review_id"`
	Outcome  string `json:"outcome"`
	ReportID string `json:"report_id,omitempty"`
	// ReviewerSession is the full session ID that produced the judgement.
	//
	// It is recorded because "an independent review approved this" is only
	// meaningful if the reader can tell who reviewed. A result that omits it is
	// asserting independence it cannot name.
	ReviewerSession string `json:"reviewer_session,omitempty"`
	// ReviewedAt is when the judgement was accepted.
	ReviewedAt string `json:"reviewed_at,omitempty"`
	// ReportSHA256 is the measured digest of the committed reasoning body.
	ReportSHA256 string `json:"report_sha256,omitempty"`
	// Trusted records whether the answer was broker-collected from the
	// reviewer's own output or imported by hand. Both are permitted; only one is
	// an independent model judgement, and a reader must be able to tell.
	Trusted bool `json:"trusted,omitempty"`
}

// ResultWorkspace describes the content an outcome applies to.
type ResultWorkspace struct {
	Branch       string   `json:"branch,omitempty"`
	BaseHead     string   `json:"base_head,omitempty"`
	SnapshotID   string   `json:"snapshot_id,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	DiffArtifact string   `json:"diff_artifact,omitempty"`
	// ContentSHA256 pins the content the outcome was measured against.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// BaselineSHA256 and FinalSHA256 are the content identities either side of
	// the work. Comparing them is what makes code_changed a measurement rather
	// than an inference from how many attempts the request took.
	BaselineSHA256 string `json:"baseline_sha256,omitempty"`
	FinalSHA256    string `json:"final_sha256,omitempty"`
}

// ResultArtifact is a digest-pinned companion to a result.
type ResultArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Status is a final outcome.
type Status string

const (
	// StatusPass means the obligations were met and verification policy
	// satisfied, and an independent completion audit passed.
	StatusPass Status = "PASS"
	// StatusFail means evidence disproved an obligation after permitted
	// recovery was exhausted.
	StatusFail Status = "FAIL"
	// StatusBlocked means required evidence, access, identity or safe recovery
	// could not be established. It is explicitly not a failure of the work.
	StatusBlocked Status = "BLOCKED"
	// StatusCancelled means cancellation was requested and its effects
	// reconciled.
	StatusCancelled Status = "CANCELLED"
	// StatusComplete means a data-only report was produced; it is not engineering PASS.
	StatusComplete Status = "COMPLETE"
)

// Terminal reports whether a status is a final outcome.
func (s Status) Terminal() bool {
	switch s {
	case StatusPass, StatusFail, StatusBlocked, StatusCancelled, StatusComplete:
		return true
	}
	return false
}

// Valid reports whether the status is one this build understands.
func (s Status) Valid() bool { return s.Terminal() }

// Finding is one thing the audit or investigation concluded.
type Finding struct {
	// Criterion is the obligation or check this finding bears on.
	Criterion string `json:"criterion,omitempty"`
	Severity  string `json:"severity,omitempty"`
	// EvidenceRefs names the immutable evidence this rests on. A finding with
	// no evidence is a conclusion, not a finding.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	Summary      string   `json:"summary"`
}
