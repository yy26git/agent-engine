package control

// The durable request store and the admission decision.
//
// Admission is the most safety-critical thing this module does, because it is
// the last point at which a request can be refused cheaply. Everything after it
// may have created sessions, spent model time and written artifacts.
//
// The whole design follows from one requirement: submitting the same request
// twice must produce one set of effects, and submitting different bytes under an
// already-used identifier must produce none. That means the request's content
// digest, not its identifier, is what identifies it — and the digest has to be
// computed from the caller's bytes before any effect exists, so a duplicate can
// be answered from the stored copy rather than by re-running anything.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// File names inside a request directory.
const (
	RequestFileName  = "REQUEST.json"
	StateFileName    = "STATE.json"
	ResultFileName   = "RESULT.json"
	ReportsDirName   = "REPORTS"
	EvidenceDirName  = "EVIDENCE"
	RequestDigestKey = "request_sha256"
)

// ErrNoState is returned when a request directory has no state.
var ErrNoState = errors.New("agent-engine: no state for this request")

// ErrConflict is returned when a request ID is reused with different content.
//
// This is a refusal, not a merge and not an overwrite. The caller is asserting
// two incompatible things about the same identifier, and the only safe response
// is to do nothing and make them say which one they meant.
var ErrConflict = errors.New("agent-engine: request_id already admitted with different content")

// ErrCorrupt is returned when a machine file cannot be read as its schema.
var ErrCorrupt = errors.New("agent-engine: machine file is corrupt, refusing to guess")

// Store is a request store rooted at an operator-approved directory.
type Store struct {
	cfg *Config
	// Now supplies the clock, so tests are deterministic.
	Now func() time.Time
}

// NewStore builds a store bound to an operator configuration.
func NewStore(cfg *Config) *Store { return &Store{cfg: cfg, Now: time.Now} }

// Config returns the operator policy in force.
func (s *Store) Config() *Config { return s.cfg }

func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Store) nowRFC3339() string { return s.now().UTC().Format(time.RFC3339) }

// Root returns the store root.
func (s *Store) Root() string { return s.cfg.StoreRoot }

// RequestDir returns the directory for one request.
func (s *Store) RequestDir(requestID string) (string, error) {
	if err := contract.ValidateRequestID(requestID); err != nil {
		return "", err
	}
	return filepath.Join(s.cfg.StoreRoot, RequestsDirName, requestID), nil
}

// Init prepares the store's fixed layout. It is idempotent.
func (s *Store) Init() error {
	for _, dir := range []string{
		s.cfg.StoreRoot,
		filepath.Join(s.cfg.StoreRoot, RequestsDirName),
		filepath.Join(s.cfg.StoreRoot, LeaseDirName),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("agent-engine: creating %s: %w", dir, err)
		}
	}
	return nil
}

// Admission is the outcome of submitting a request.
type Admission struct {
	// RequestID is the admitted identity.
	RequestID string `json:"request_id"`
	// RequestDigest is the content digest that identifies it.
	RequestDigest string `json:"request_sha256"`
	// TaskID is the stable task identity.
	TaskID string `json:"task_id,omitempty"`
	// Stage is the stage the request is now in.
	Stage string `json:"stage"`
	// Accepted is false when this submission matched an existing request, in
	// which case nothing new was started.
	Accepted bool `json:"accepted"`
	// Duplicate is true when an identical request was already admitted.
	Duplicate bool `json:"duplicate"`
	// Store is the canonical store the request lives in, echoed so a caller
	// cannot be confused about which store answered.
	Store string `json:"store"`
	// PolicyDigest is the operator policy admitted under.
	PolicyDigest string `json:"policy_digest"`
	// Limits is the effective ceiling for this request.
	Limits Limits `json:"limits"`
	// Path is the request directory.
	Path string `json:"path"`
}

// Admit accepts a request exactly once.
//
// The sequence is deliberate. Everything that can refuse the request is checked
// before the store lock is taken and before anything is written, so a rejected
// request leaves no trace at all. Then the lock is taken, the existing request
// is consulted, and only a genuinely new request is written.
//
// Writing REQUEST.json and STATE.json under one lock is what makes the two
// consistent. If they could be written separately, a crash between them would
// leave a request whose durable identity exists but whose state does not, and
// the next submission of the same bytes would be unable to tell whether it was
// a first run or a recovery.
func (s *Store) Admit(req *contract.Request) (*Admission, error) {
	// Validation and policy checks are cheap and hold no lock, so they run before
	// one is taken.
	if err := req.Validate(); err != nil {
		return nil, err
	}
	digest, err := contract.CanonicalSHA256(req)
	if err != nil {
		return nil, err
	}
	if err := s.validateAgainstPolicy(req); err != nil {
		return nil, err
	}
	if err := s.Init(); err != nil {
		return nil, err
	}

	dir, err := s.RequestDir(req.RequestID)
	if err != nil {
		return nil, err
	}

	unlock, err := s.storeLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.admitLocked(req, dir, digest)
}

// admitLocked admits a request with the store lock ALREADY HELD.
//
// It exists so a compound operation — admitting a root and its children, which
// must be atomic or nothing — can be one transaction. The store lock is a file
// lock and is not reentrant, so calling Admit from inside a held lock would
// deadlock rather than nest.
func (s *Store) admitLocked(req *contract.Request, dir, digest string) (*Admission, error) {
	return s.admitLockedWithProfiles(req, dir, digest, nil)
}

func (s *Store) admitLockedWithProfiles(req *contract.Request, dir, digest string, profiles map[ProfileRole]ProfileRef) (*Admission, error) {
	return s.admitLockedWithInitialState(req, dir, digest, profiles, nil)
}

func (s *Store) admitLockedWithInitialState(req *contract.Request, dir, digest string, profiles map[ProfileRole]ProfileRef, initialize func(*State) error) (*Admission, error) {
	// An existing request is answered from the stored copy. Nothing is created
	// and nothing is sent.
	if existing, err := s.loadRequest(dir); err == nil {
		if existing.RequestDigest != digest {
			return nil, fmt.Errorf("%w: %s was admitted with digest %s, this submission has %s",
				ErrConflict, req.RequestID, existing.RequestDigest, digest)
		}
		st, err := s.loadState(dir)
		if err != nil {
			return nil, fmt.Errorf("%w: request admission has no readable state: %v", ErrCorrupt, err)
		}
		adm := &Admission{
			RequestID:     existing.Request.RequestID,
			RequestDigest: existing.RequestDigest,
			TaskID:        existing.TaskID,
			Accepted:      false,
			Duplicate:     true,
			Store:         s.cfg.StoreRoot,
			PolicyDigest:  existing.PolicyDigest,
			Limits: Limits{
				RepairCycles:  existing.EffectiveLimits.RepairCycles,
				WorkerGens:    existing.EffectiveLimits.WorkerGens,
				AuditAttempts: existing.EffectiveLimits.AuditAttempts,
			},
			Path: dir,
		}
		if st != nil {
			adm.Stage = string(st.Stage)
		}
		return adm, nil
	} else if !errors.Is(err, ErrNoState) {
		return nil, err
	}

	now := s.nowRFC3339()
	st := newAdmittedState(req, digest, s.cfg, s.validateAgainstPolicy, now)
	st.ExecutionProfiles = profiles
	if initialize != nil {
		if err := initialize(st); err != nil {
			return nil, err
		}
	}

	// The immutable admitted request. The digest is taken over the bare request
	// alone, so re-submitting the caller's bytes recomputes the same identity.
	//
	// There is deliberately no store-wide index. A request is located by its own
	// directory and nothing else, so a lookup can never depend on the current
	// working directory, and there is no second database that can disagree with
	// the request directories about what was admitted.
	admittedFile := admittedRequest{
		Request:         *req,
		RequestDigest:   digest,
		TaskID:          st.TaskID,
		PolicyDigest:    s.cfg.Digest,
		EffectiveLimits: contract.Limits{RepairCycles: st.Limits.RepairCycles, WorkerGens: st.Limits.WorkerGens, AuditAttempts: st.Limits.AuditAttempts},
		AdmittedAt:      now,
	}
	if err := writeJSONDurable(s.cfg.StoreRoot, requestRel(req.RequestID, RequestFileName), admittedFile, 0o444); err != nil {
		return nil, err
	}

	if err := s.saveState(dir, st); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, ReportsDirName), 0o755); err != nil {
		return nil, fmt.Errorf("agent-engine: creating reports dir: %w", err)
	}

	return &Admission{
		RequestID:     req.RequestID,
		RequestDigest: digest,
		TaskID:        st.TaskID,
		Stage:         string(st.Stage),
		Accepted:      true,
		Duplicate:     false,
		Store:         s.cfg.StoreRoot,
		PolicyDigest:  s.cfg.Digest,
		Limits:        st.Limits,
		Path:          dir,
	}, nil
}

// admittedRequest is REQUEST.json: the immutable admitted input plus the identity
// and policy it was admitted under.
//
// The request is nested under its own key rather than embedded. Embedding would
// let the outer Limits field shadow the request's own Limits in JSON, silently
// dropping whichever the two disagreed about — and a dropped limit is a limit
// nobody enforced.
type admittedRequest struct {
	Request         contract.Request `json:"request"`
	RequestDigest   string           `json:"request_sha256"`
	TaskID          string           `json:"task_id,omitempty"`
	PolicyDigest    string           `json:"policy_digest"`
	EffectiveLimits contract.Limits  `json:"effective_limits"`
	AdmittedAt      string           `json:"admitted_at"`
}

func requestRel(requestID, name string) string {
	return filepath.Join(RequestsDirName, requestID, name)
}

func newAdmittedState(req *contract.Request, digest string, cfg *Config, validate func(*contract.Request) error, now string) *State {
	limits := req.Limits.Effective()
	st := &State{
		SchemaVersion: CurrentSchemaVersion,
		Revision:      0,
		RequestID:     req.RequestID,
		RequestDigest: digest,
		PolicyDigest:  cfg.Digest,
		TaskID:        "T-" + digest[:16],
		Stage:         StageReady,
		StageReason:   "admitted; no effects started",
		Workspace:     req.Workspace.Path,
		Profile:       cfg.Profile,
		// The authority is copied in at admission so a later change to
		// CONTROL.toml cannot retroactively grant or revoke who was allowed to
		// accept a decision during this request's life.
		DecisionAuthorityRole: cfg.DecisionAuthority,
		Generation:            1,
		Checks:                append([]string(nil), checkIDs(req)...),
		Limits: Limits{
			RepairCycles:   limits.RepairCycles,
			WorkerGens:     limits.WorkerGens,
			AuditAttempts:  limits.AuditAttempts,
			GuidanceRounds: cfg.Limits.GuidanceRounds,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, o := range req.Obligations {
		st.Obligations = append(st.Obligations, Obligation{
			ID:               o.ID,
			Criterion:        o.Criterion,
			RequiredChecks:   append([]string(nil), o.RequiredChecks...),
			Disposition:      DispositionOpen,
			OriginGeneration: 1,
		})
	}
	return st
}

func checkIDs(req *contract.Request) []string {
	out := make([]string, 0, len(req.Verification))
	for _, v := range req.Verification {
		out = append(out, v.CheckID)
	}
	return out
}

// validateAgainstPolicy checks a request against operator-owned policy.
//
// This is where a request stops being a document and becomes an admissible
// instruction. Every limit is checked downward, every named check must exist in
// the operator registry, the workspace must be inside an approved root, and
// every evidence path must be inside an approved evidence root. A request that
// fails any of these is refused whole.
func (s *Store) validateAgainstPolicy(req *contract.Request) error {
	if _, ok := s.cfg.ApprovedSourceRoot(req.Workspace.Path); !ok {
		return fmt.Errorf("agent-engine: workspace %q is not inside an approved source root (%s)",
			req.Workspace.Path, strings.Join(s.cfg.SourceRoots, ", "))
	}
	eff := req.Limits.Effective()
	if err := s.cfg.WithinLimits("repair_cycles", eff.RepairCycles, s.cfg.Limits.RepairCycles); err != nil {
		return err
	}
	if err := s.cfg.WithinLimits("worker_generations", eff.WorkerGens, s.cfg.Limits.WorkerGens); err != nil {
		return err
	}
	if err := s.cfg.WithinLimits("audit_attempts", eff.AuditAttempts, s.cfg.Limits.AuditAttempts); err != nil {
		return err
	}
	// A check the operator has not defined cannot be run, and a required check
	// that never runs must not be treated as satisfied.
	for _, v := range req.Verification {
		if _, err := s.cfg.Check(v.CheckID); err != nil {
			return err
		}
	}
	for _, o := range req.Obligations {
		for _, c := range o.RequiredChecks {
			if _, err := s.cfg.Check(c); err != nil {
				return fmt.Errorf("agent-engine: obligation %q requires unknown check %q", o.ID, c)
			}
		}
	}
	for _, e := range req.Evidence {
		if _, ok := s.cfg.ApprovedEvidenceRoot(e.Path); !ok {
			return fmt.Errorf("agent-engine: evidence %q is not inside an approved evidence root (%s)",
				e.Path, strings.Join(s.cfg.EvidenceRoots, ", "))
		}
	}
	return nil
}

// loadRequest reads the admitted request from a request directory.
func (s *Store) loadRequest(dir string) (*admittedRequest, error) {
	var adm admittedRequest
	if err := readJSONStrict(filepath.Join(dir, RequestFileName), &adm); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoState
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, RequestFileName, err)
	}
	return &adm, nil
}

// loadRequestDir reads the admitted request file for an ID.
func (s *Store) loadRequestDir(requestID string) (*admittedRequest, error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	return s.loadRequest(dir)
}

// Request returns the admitted request for an ID.
func (s *Store) Request(requestID string) (*contract.Request, error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	adm, err := s.loadRequest(dir)
	if err != nil {
		return nil, err
	}
	r := adm.Request
	return &r, nil
}
func (s *Store) loadState(dir string) (*State, error) {
	raw, err := os.ReadFile(filepath.Join(dir, StateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoState
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, StateFileName, err)
	}
	var st State
	if derr := readJSONStrict(filepath.Join(dir, StateFileName), &st); derr != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, StateFileName, derr)
	}
	// Preserve the original bytes BEFORE migrating them in memory, so a crash
	// during a later commit still leaves the pre-migration state on disk.
	if st.SchemaVersion > 0 && st.SchemaVersion < CurrentSchemaVersion {
		if _, eerr := s.exportPreMigrationState(filepath.Base(dir), raw); eerr != nil {
			return nil, eerr
		}
	}
	if err := s.migrateAndValidate(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) migrateAndValidate(st *State) error {
	if st.SchemaVersion == 0 {
		// Tolerate a pre-versioning file rather than discarding a live request.
		st.SchemaVersion = CurrentSchemaVersion
	}
	if st.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("agent-engine: state is schema version %d but this build understands at most %d; refusing to downgrade",
			st.SchemaVersion, CurrentSchemaVersion)
	}
	if st.SchemaVersion == StateSchemaV1 {
		migrateStateV1ToV2(st)
	}
	if st.SchemaVersion == StateSchemaV2 {
		migrateStateV2ToV3(st)
	}
	if st.SchemaVersion == 3 {
		// No historical notifications are inferred. The next write records the
		// current projection; read-only observers bootstrap from a snapshot.
		st.Progress = nil
		st.SchemaVersion = 4
	}
	if st.SchemaVersion == 4 {
		if st.CancellationRequested && st.CancellationRequestedAt == "" {
			st.CancellationRequestedAt = st.UpdatedAt
		}
		st.SchemaVersion = 5
	}
	if st.SchemaVersion == 5 {
		// Existing episodes have no imported source plan. Never infer one from
		// a generated plan or change its authorization during migration.
		st.SchemaVersion = 6
	}
	if st.SchemaVersion == 6 {
		st.SchemaVersion = 7
	}
	return st.validate()
}

// migrationSidecarName is where the pre-migration bytes are preserved.
//
// The export is written once per schema upgrade and is never edited, so a later
// reader can hash it and confirm the migrated state descends from exactly those
// bytes. Losing that link means a migration cannot be audited at all: the only
// record would be the migrated state, which is what we were trying to check.
const migrationSidecarName = "MIGRATION-STATE-v1.json"

// exportPreMigrationState preserves a v1 state before it is migrated in place.
//
// It returns the path it wrote, or an empty string when there was nothing to
// preserve. A failure to preserve is an error rather than a silent skip: silently
// migrating is how provenance gets lost.
func (s *Store) exportPreMigrationState(requestID string, original []byte) (string, error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return "", err
	}
	// Only the pre-migration bytes are preserved, and only when the file still
	// says v1. If the sidecar already exists, the original is already preserved
	// and rewriting it would replace the historical bytes with a later migration.
	st, serr := s.readJSONUnversioned(filepath.Join(dir, StateFileName))
	if serr == nil && st.SchemaVersion >= StateSchemaV1 && st.SchemaVersion != 0 {
		// Already migrated once; the sidecar from that migration stands.
		if _, statErr := os.Stat(filepath.Join(dir, migrationSidecarName)); statErr == nil {
			return migrationSidecarName, nil
		}
	}
	rel := filepath.Join(RequestsDirName, requestID, migrationSidecarName)
	if err := writeDurable(s.cfg.StoreRoot, rel, original, 0o444); err != nil {
		return "", fmt.Errorf("%w: the pre-migration state could not be preserved: %v", ErrMigrationBlocked, err)
	}
	return migrationSidecarName, nil
}

// ErrMigrationBlocked is returned when a schema upgrade cannot preserve its source.
var ErrMigrationBlocked = errors.New("agent-engine: state migration is blocked")

// migrateStateV1ToV2 fills the completion-evidence fields a v1 state cannot have.
//
// It migrates in memory only. Nothing here writes to disk: the caller that
// commits is responsible for persisting the new version, and the original bytes
// stay on disk until that commit succeeds. That ordering is what makes a crash
// during migration a no-op rather than a half-converted authority record.
//
// The one thing this deliberately does NOT do is reconstruct provenance. A v1
// state with a committed PASS has no record of which session approved it, so
// Completion stays nil and validate() refuses to let that state be re-published
// as a new approval. The historical result remains readable with its original
// assurance level; it simply cannot be extended.
func migrateStateV1ToV2(st *State) {
	// The current worker is a known implementer even in a v1 state.
	st.NoteImplementation(st.WorkerSession)
	// A v1 state that already committed a terminal PASS keeps its digest, but
	// gains no evidence record. That asymmetry is the point: the outcome stays
	// exactly as it was written, and nothing new is asserted about it.
	// Advance to THIS migration's target, not to CurrentSchemaVersion.
	//
	// Writing the current version here is a latent skip: the next migration in the
	// chain tests `SchemaVersion < CurrentSchemaVersion` and would find it already
	// satisfied, so a v2 state would migrate to v2 and then be persisted as if it
	// had reached v3. Every migration must name the version it produces.
	st.SchemaVersion = StateSchemaV2
}

// State returns the current state for a request.
func (s *Store) State(requestID string) (*State, error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	return s.loadState(dir)
}

// Update performs a locked read-modify-write on a request's state.
//
// This is the only mutator. External I/O — sending to a session, running a
// check, calling a model — must never happen inside fn, because the lock is held
// for the whole transaction and a slow fn turns ordinary contention into a
// failed decision. The pattern for anything expensive is: read a snapshot,
// compute, then commit through Update and revalidate the revision.
func (s *Store) Update(requestID string, fn func(*State) error) error {
	_, err := s.UpdateCommitted(requestID, fn)
	return err
}

// UpdateCommitted is Update that returns the committed state.
func (s *Store) UpdateCommitted(requestID string, fn func(*State) error) (*State, error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	unlock, err := s.storeLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.updateCommittedLocked(requestID, dir, fn)
}

// updateCommittedLocked commits a state change with the store lock ALREADY HELD.
//
// It is the same split as admitLocked, for the same reason: a compound operation
// that must be one transaction — admitting a workflow root, its immutable record
// and every child — cannot call UpdateCommitted while holding the lock, because
// the file lock is not reentrant and the call would deadlock against itself.
func (s *Store) updateCommittedLocked(requestID, dir string, fn func(*State) error) (*State, error) {
	st, err := s.loadState(dir)
	if err != nil {
		return nil, err
	}
	progress := st.Progress.clone()
	programBindingBefore := cloneProgramBinding(st.ProgramBinding)
	acceptancesBefore := append([]ProgramPlanAcceptanceReceipt(nil), st.ProgramAcceptances...)
	if err := fn(st); err != nil {
		return nil, err
	}
	if !sameProgramBindingValue(programBindingBefore, st.ProgramBinding) {
		return nil, fmt.Errorf("agent-engine: an admitted program definition and card binding are immutable")
	}
	if len(st.ProgramAcceptances) < len(acceptancesBefore) {
		return nil, fmt.Errorf("agent-engine: program acceptance receipts are append-only")
	}
	for i := range acceptancesBefore {
		if st.ProgramAcceptances[i] != acceptancesBefore[i] {
			return nil, fmt.Errorf("agent-engine: program acceptance receipts are append-only")
		}
	}
	// Callers mutate engineering state, never the derived notification ledger.
	st.Progress = progress
	if err := s.saveState(dir, st); err != nil {
		return nil, err
	}
	return st.Clone(), nil
}

func cloneProgramBinding(in *EpisodeProgramBinding) *EpisodeProgramBinding {
	if in == nil {
		return nil
	}
	out := *in
	out.Definition = *cloneProgramDefinition(&in.Definition)
	if in.RepairRoutes != nil {
		out.RepairRoutes = make([]contract.RepairRoute, len(in.RepairRoutes))
		copy(out.RepairRoutes, in.RepairRoutes)
	}
	if in.Resources != nil {
		out.Resources = make([]contract.ResourceClaim, len(in.Resources))
		copy(out.Resources, in.Resources)
	}
	return &out
}

func sameProgramBindingValue(a, b *EpisodeProgramBinding) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	left, err1 := json.Marshal(a)
	right, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(left) == string(right)
}

// saveState validates, advances the revision and commits state.json.
//
// The revision is derived here rather than accepted from the caller, so no
// writer can stall the counter and two different states can never claim the same
// revision.
func (s *Store) saveState(dir string, st *State) error {
	if st == nil {
		return errors.New("agent-engine: refusing to save nil state")
	}
	if st.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("agent-engine: refusing to write schema version %d from a build that writes %d",
			st.SchemaVersion, CurrentSchemaVersion)
	}
	st.SchemaVersion = CurrentSchemaVersion
	if st.CancellationRequested && st.CancellationRequestedAt == "" {
		st.CancellationRequestedAt = s.nowRFC3339()
	}
	if st.ActiveReview != nil && st.ActiveReview.Deadline == "" {
		st.ActiveReview.Deadline = st.reviewDeadline(s.cfg)
	}
	if st.Revision < 1 {
		st.Revision = 1
	}
	st.Revision++
	st.UpdatedAt = s.nowRFC3339()
	if st.CreatedAt == "" {
		st.CreatedAt = st.UpdatedAt
	}
	if err := st.recordProgress(); err != nil {
		return err
	}
	if err := st.validate(); err != nil {
		return err
	}
	return writeJSONDurable(s.cfg.StoreRoot,
		filepath.Join(RequestsDirName, filepath.Base(dir), StateFileName), st, 0o600)
}

// ScratchStore copies one request's directory into a disposable location and
// returns a store rooted at the copy.
//
// This is how a dry run is made structurally incapable of writing real state.
// The embedded draft's dry run suppressed sends but still rewrote state,
// advanced the cursor and consumed attempt budget, so any test using it was
// mutating the workflow it claimed to observe. Suppressing the writes you
// remember is not the same as being unable to make them, and this is the same
// discipline the CLI's dry run uses.
func (s *Store) ScratchStore(requestID string) (*Store, func(), error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, nil, fmt.Errorf("agent-engine: %w", err)
	}
	tmp, err := os.MkdirTemp("", "agent-engine-dryrun-")
	if err != nil {
		return nil, nil, fmt.Errorf("agent-engine: creating dry-run scratch: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	dst := filepath.Join(tmp, "store", RequestsDirName, requestID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("agent-engine: preparing dry-run scratch: %w", err)
	}
	if err := copyTreeForScratch(dir, dst); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("agent-engine: copying request for dry run: %w", err)
	}

	cfg := *s.cfg
	cfg.StoreRoot = filepath.Join(tmp, "store")
	scoped := NewStore(&cfg)
	scoped.Now = s.Now
	return scoped, cleanup, nil
}

// copyTreeForScratch copies a request directory, skipping locks and temp files.
func copyTreeForScratch(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() || strings.HasSuffix(path, ".lock") || strings.Contains(filepath.Base(path), ".tmp") {
			// Locks and staging files are runtime artefacts, not state.
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

// storeLock takes the store-wide admission lock.
//
// It is separate from the state lock, which is held only for one
// read-modify-write. Two concurrent admissions would otherwise each take the
// state lock in turn, both observe "no such request", and both create it — the
// exact duplicate-intent problem admission exists to prevent.
func (s *Store) storeLock() (func(), error) {
	if err := os.MkdirAll(s.cfg.StoreRoot, 0o755); err != nil {
		return nil, fmt.Errorf("agent-engine: creating store root: %w", err)
	}
	path := filepath.Join(s.cfg.StoreRoot, StoreLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("agent-engine: opening store lock: %w", err)
	}
	deadline := time.Now().Add(storeLockTimeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("agent-engine: store lock busy after %s: %w", storeLockTimeout, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// storeLockTimeout bounds how long an admission waits for a competing writer.
const storeLockTimeout = 10 * time.Second

// readJSONUnversioned decodes a state file for inspection without migrating it.
//
// It exists so the migration sidecar decision can look at the ORIGINAL schema
// version, which a migrated in-memory copy would already have overwritten.
func (s *Store) readJSONUnversioned(path string) (*State, error) {
	var st State
	if err := readJSONStrict(path, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// AdmitEpisode admits an episode and installs its phase root.
//
// It REUSES the ordinary admission path rather than adding a parallel one. A
// second admission would be a second set of guarantees, and the set that drifts
// is the one that ships; everything EpisodeRequest asks for — validation, the
// immutable request bytes, capability preflight, limits — is already enforced by
// Admit and must not be enforced a second time with a second opinion.
//
// What AdmitEpisode adds is the phase root, template binding and optional local
// pilot grant in the initial state commit. An episode that failed admission leaves no phase
// record, because there is no episode.
//
// The template's roles are resolved BEFORE admission and bound IN THE SAME WRITE
// as the phase root, so "admitted" and "bound" are one fact rather than two
// with a crash window between them. An episode that cannot bind every role it
// declares is refused and commits nothing.
func (s *Store) AdmitEpisode(ep *contract.EpisodeRequest) (*Admission, error) {
	return s.admitEpisodeWithProgram(ep, false, nil)
}

// ValidateEpisode applies fresh admission policy without creating requests, locks,
// artifacts or processes. It is a preview; admission rechecks policy at commit.
func (s *Store) ValidateEpisode(ep *contract.EpisodeRequest) error {
	_, err := s.admitEpisodeWithProgram(ep, true, nil)
	return err
}

func (s *Store) admitEpisodeWithProgram(ep *contract.EpisodeRequest, preview bool, programBinding *EpisodeProgramBinding) (*Admission, error) {
	if ep == nil {
		return nil, fmt.Errorf("%w: no episode request supplied", contract.ErrEpisodeInvalid)
	}
	if err := contract.ValidateImportedPlanDocument(ep.ImportedPlan); err != nil {
		return nil, err
	}
	episodeBytes, err := json.Marshal(ep)
	if err != nil {
		return nil, err
	}
	episodeDigest := sha256Hex(episodeBytes)
	dir, err := s.RequestDir(ep.EpisodeID)
	if err != nil {
		return nil, err
	}
	// A duplicate is an observation, not fresh authorization or a state rewrite.
	// Read it under the store lock before consulting changed/expired live grants.
	if _, err := s.loadState(dir); !preview && err == nil {
		unlock, err := s.storeLock()
		if err != nil {
			return nil, err
		}
		defer unlock()
		existing, err := s.loadState(dir)
		if err != nil {
			return nil, err
		}
		if existing.EpisodeRequestSHA256 != episodeDigest {
			return nil, fmt.Errorf("%w: episode content/template differs or has no pinned episode digest", ErrConflict)
		}
		if !sameProgramAdmission(existing.ProgramBinding, programBinding) {
			return nil, fmt.Errorf("%w: episode program binding differs from its admitted identity", ErrConflict)
		}
		request, err := s.loadRequest(dir)
		if err != nil {
			return nil, err
		}
		return s.admitLocked(&request.Request, dir, request.RequestDigest)
	} else if err != nil && !errors.Is(err, ErrNoState) {
		return nil, err
	}

	// The template is resolved against LOCALLY approved policy, and an unknown
	// name is refused rather than defaulted. Defaulting would run the episode
	// under a shape nobody approved, which is the whole thing templates exist to
	// prevent.
	template, ok := s.cfg.EpisodeTemplates.Lookup(ep.Template)
	if !ok {
		return nil, fmt.Errorf("%w: template %q is not approved by this operator", contract.ErrEpisodeInvalid, ep.Template)
	}
	if ep.ImportedPlan != nil && (!template.AllowsPhase(PhasePlan) || !template.AllowsPhase(PhasePlanAccept)) {
		return nil, fmt.Errorf("%w: importing a plan requires a template with PLAN review and PLAN_ACCEPT", contract.ErrEpisodeInvalid)
	}
	for _, checkID := range template.RequiredChecks {
		if _, err := s.cfg.Check(checkID); err != nil {
			return nil, fmt.Errorf("%w: template %q requires an unavailable check: %v", contract.ErrEpisodeInvalid, template.Name, err)
		}
	}
	if !template.RequiresReviewer() && !template.ReportOnly {
		return nil, fmt.Errorf("%w: template %q binds no reviewer, and an episode nothing can audit "+
			"would never be able to reach a verdict", contract.ErrEpisodeInvalid, ep.Template)
	}
	var repairBranch, repairHead, repairCommon string
	var repairBundle *RepairBundleReceipt
	if template.RepairBranchPrefix != "" {
		if ep.RepairBundleSHA256 != "" {
			receipt, err := s.loadRepairBundleForAdmission(ep.EpisodeID, ep.Template, ep.Workspace.Path, ep.RepairBundleSHA256)
			if err != nil {
				return nil, err
			}
			repairBundle = &receipt
		} else {
			var err error
			repairBranch, repairHead, repairCommon, err = repairGitIdentity(ep.Workspace.Path)
			if err != nil {
				return nil, err
			}
			if repairBranch != RepairBranchName(template.RepairBranchPrefix, ep.EpisodeID) {
				return nil, fmt.Errorf("agent-engine: repair requires its dedicated episode branch; run prepare-repair before admission")
			}
		}
	} else if ep.RepairBundleSHA256 != "" {
		return nil, fmt.Errorf("agent-engine: a repair bundle requires a locally approved repair-branch template")
	}

	// The roles are bound BEFORE anything is written, so a template this
	// operator cannot actually run is refused without leaving an admitted
	// request behind. Binding after Admit would still be fail-closed for the
	// STATE.json, but it would leave an ordinary request on disk that was
	// never going to become an episode.
	pilot, err := s.localPilotFor(ep, template)
	if err != nil {
		return nil, err
	}
	bound, berr := s.episodeRoleProfilesForPilot(template, ep.Workspace.Path, pilot)
	if berr != nil {
		return nil, berr
	}
	var selectedProgramCard *contract.ProgramCard
	var programBaseline Snapshot
	if programBinding != nil {
		var ok bool
		selectedProgramCard, ok = programCard(&programBinding.Definition, programBinding.CardID)
		if !ok {
			return nil, fmt.Errorf("agent-engine: program card is missing")
		}
		programBaseline, err = measureStableEpisodeSource(ep.Workspace.Path, s.cfg.StoreRoot, s.now())
		if err != nil {
			return nil, err
		}
		dependencyHash, derr := measureDependencyBaseline(ep.Workspace.Path)
		if derr != nil {
			return nil, derr
		}
		if err := validateProgramEpisodeAdmission(programBinding, selectedProgramCard, ep, template, bound, programBaseline, dependencyHash); err != nil {
			return nil, err
		}
	}

	if template.ReportOnly {
		if ep.StructuredTask == nil || ep.ImportedPlan != nil || ep.Constraints.AllowCodeChanges || len(ep.Verification) > 0 || ep.RepairBundleSHA256 != "" {
			return nil, fmt.Errorf("report-only requires a structured_task and forbids mutation, checks, imported plans and repair bundles")
		}
		if err := ep.StructuredTask.Validate(); err != nil {
			return nil, err
		}
	} else if ep.StructuredTask != nil {
		return nil, fmt.Errorf("structured tasks require an approved report-only template")
	}
	// The episode becomes an ordinary admitted request. Its operation follows
	// the template's own grant: a read-only template investigates and nothing
	// else, so it cannot smuggle a repair request in behind a read-only label.
	op := contract.OpInvestigate
	if !template.ReadOnly {
		op = contract.OpRepair
	}
	req := contract.Request{
		Version:      contract.Version,
		RequestID:    ep.EpisodeID,
		Origin:       ep.Origin,
		Operation:    op,
		Workspace:    ep.Workspace,
		Objective:    ep.Objective,
		Obligations:  episodeObligations(ep.Obligations),
		Constraints:  ep.Constraints,
		Verification: ep.Verification,
		Limits:       ep.Limits,
	}
	// A read-only template cannot carry a mutation grant, whatever the caller
	// asked for. The template is the authority, not the request.
	if template.ReadOnly {
		req.Constraints.AllowCodeChanges = false
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := s.validateAgainstPolicy(&req); err != nil {
		return nil, err
	}
	digest, err := contract.CanonicalSHA256(&req)
	if err != nil {
		return nil, err
	}
	if preview {
		// Existing identities must still be checked without changing their state.
		existing, err := s.loadState(dir)
		if err != nil && !errors.Is(err, ErrNoState) {
			return nil, err
		}
		if err == nil && existing.EpisodeRequestSHA256 != episodeDigest {
			return nil, fmt.Errorf("%w: episode content/template differs", ErrConflict)
		}
		if err == nil && !sameProgramAdmission(existing.ProgramBinding, programBinding) {
			return nil, fmt.Errorf("%w: episode program binding differs from its admitted identity", ErrConflict)
		}
		return nil, nil
	}
	if err := s.Init(); err != nil {
		return nil, err
	}
	unlock, err := s.storeLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if programBinding != nil {
		pinned, err := s.VerifyProgramAuthority(&programBinding.Definition)
		if err != nil {
			return nil, fmt.Errorf("agent-engine: local program authority changed before episode admission: %w", err)
		}
		if pinned != programBinding.ProgramDigest {
			return nil, fmt.Errorf("agent-engine: local program authority changed before episode admission")
		}
	}
	if repairBundle != nil {
		fresh, err := s.loadRepairBundleForAdmission(ep.EpisodeID, ep.Template, ep.Workspace.Path, ep.RepairBundleSHA256)
		if err != nil {
			return nil, fmt.Errorf("agent-engine: repair bundle changed before admission: %w", err)
		}
		if fresh.ReceiptSHA != repairBundle.ReceiptSHA {
			return nil, fmt.Errorf("agent-engine: repair bundle receipt changed before admission")
		}
		repairBundle = &fresh
	}
	if existing, err := s.loadState(dir); err == nil {
		if existing.EpisodeRequestSHA256 != episodeDigest {
			return nil, fmt.Errorf("%w: episode identity/template differs or has no pinned episode digest", ErrConflict)
		}
		if !sameProgramAdmission(existing.ProgramBinding, programBinding) {
			return nil, fmt.Errorf("%w: episode program binding differs from its admitted identity", ErrConflict)
		}
	} else if !errors.Is(err, ErrNoState) {
		return nil, err
	}
	if programBinding != nil {
		fresh, err := measureStableEpisodeSource(ep.Workspace.Path, s.cfg.StoreRoot, s.now())
		if err != nil {
			return nil, err
		}
		dependencyHash, err := measureDependencyBaseline(ep.Workspace.Path)
		if err != nil {
			return nil, err
		}
		if fresh.ContentDigest != programBaseline.ContentDigest || dependencyHash != programBinding.DependencyHash {
			return nil, fmt.Errorf("agent-engine: source or dependency baseline changed before program episode admission")
		}
	}
	budget := DefaultEpisodeBudget()
	if template.MaxWallSeconds > 0 {
		budget.WallSeconds = template.MaxWallSeconds
	}
	if template.MaxRepairIterations > 0 {
		budget.RepairIterations = template.MaxRepairIterations
	}
	if pilot != nil {
		budget.WallSeconds = min(budget.WallSeconds, pilot.MaxWallSeconds)
		budget.RepairIterations = min(budget.RepairIterations, pilot.MaxRepairIterations)
	}
	if programBinding != nil {
		if selectedProgramCard.Budget.RepairCycles > budget.RepairIterations || selectedProgramCard.Budget.AuditAttempts > budget.CompletionAudits {
			return nil, fmt.Errorf("agent-engine: program card budgets exceed the effective locally approved episode budget")
		}
		budget.RepairIterations = min(budget.RepairIterations, selectedProgramCard.Budget.RepairCycles)
		budget.CompletionAudits = min(budget.CompletionAudits, selectedProgramCard.Budget.AuditAttempts)
		budget.WorkerTurns = selectedProgramCard.Budget.WorkerTurns
	}
	return s.admitLockedWithInitialState(&req, dir, digest, bound, func(cur *State) error {
		cur.EpisodeTemplate = ep.Template
		cur.EpisodeRequestSHA256 = episodeDigest
		cur.RepairBranch, cur.RepairBaseHead, cur.RepairGitCommonDir = repairBranch, repairHead, repairCommon
		if repairBundle != nil {
			cur.RepairBundleSHA256 = repairBundle.ReceiptSHA
			cur.RepairBundleTargets = append([]RepairBundleTargetReceipt(nil), repairBundle.Targets...)
		}
		if pilot != nil {
			grant := *pilot
			cur.LocalPilot = &grant
			cur.ExecutionAssurance = AssuranceTrustedLocal
			cur.Workspace = grant.Workspace
			cur.Limits.RepairCycles = min(cur.Limits.RepairCycles, grant.MaxRepairIterations)
		}
		if programBinding != nil {
			copyBinding := *programBinding
			copyBinding.Definition = *cloneProgramDefinition(&programBinding.Definition)
			copyBinding.RepairRoutes = append([]contract.RepairRoute{}, programBinding.RepairRoutes...)
			copyBinding.Resources = append([]contract.ResourceClaim{}, programBinding.Resources...)
			cur.ProgramBinding = &copyBinding
		}
		if err := cur.StartEpisode(ep.EpisodeID, budget, s.now()); err != nil {
			return err
		}
		cur.Episode.ReportOnly = template.ReportOnly
		if ep.StructuredTask != nil {
			task := *ep.StructuredTask
			cur.Episode.StructuredTask = &task
		}
		if ep.ImportedPlan != nil {
			document := *ep.ImportedPlan
			cur.Episode.ImportedPlan = &document
		}
		return nil
	})
}

func sameProgramAdmission(existing, requested *EpisodeProgramBinding) bool {
	if existing == nil || requested == nil {
		return existing == nil && requested == nil
	}
	return existing.ProgramDigest == requested.ProgramDigest && existing.CardID == requested.CardID && existing.CardDigest == requested.CardDigest
}

// episodeObligations converts episode obligations into the admitted request's
// own shape.
//
// The conversion is a RENAME of identity, not a reinterpretation: an episode
// obligation's criterion is the obligation's criterion, and nothing here decides
// whether it has been met.
func episodeObligations(in []contract.WorkflowObligation) []contract.Obligation {
	if len(in) == 0 {
		return nil
	}
	out := make([]contract.Obligation, 0, len(in))
	for _, o := range in {
		out = append(out, contract.Obligation{
			ID: o.ID, Criterion: o.Criterion, RequiredChecks: o.RequiredChecks,
		})
	}
	return out
}
