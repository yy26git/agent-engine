package control

// The engine: one active owner per request, following the public event stream.
//
// The requirement this stage exists to satisfy is "a real recipient gets one
// instruction promptly", and each half of that sentence is a failure mode this
// file is built to avoid:
//
//   - "one instruction" — a single dispatcher owns every send, and the previous
//     embedded draft's two competing sinks are gone with the draft.
//   - "promptly" — effects are dispatched while the follow is still live, not on
//     the way out. The draft drained only after the subscription returned, so an
//     indefinite run could sit for hours recording decisions and delivering none.
//
// Single ownership is enforced by a lock, not by convention. Two engines on one
// request would interleave decisions legally and both believe they were alone.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/runtime"
)

// OwnershipLockName guards one active engine per request.
const OwnershipLockName = "OWNERSHIP.lock"

// ErrOwnershipHeld is returned when a second engine tries to own a request.
var ErrOwnershipHeld = errors.New("agent-engine: another engine already owns this request")

// ErrLegacyState is returned when a request directory carries legacy embedded
// workflow state.
//
// The legacy state is deliberately refused rather than migrated. It records
// completion and audit conclusions that were never correlated to a request, a
// generation or a reviewer, and adopting them would let an approval that was
// never actually given become the new engine's starting truth.
var ErrLegacyState = errors.New("agent-engine: legacy embedded-workflow state present and not adopted")

// EngineOptions configures one run.
type EngineOptions struct {
	// Once processes the current backlog through a captured boundary, then exits.
	//
	// The boundary is captured now, not approximated with a sleep, so the same
	// command always does the same amount of work — including when the backlog is
	// empty or contains nothing relevant.
	Once bool
	// DryRun runs the real decision and state machine against a disposable
	// request directory, with the transport replaced by a printer.
	DryRun bool
	// Log receives operational lines.
	Log func(string)
	// Now supplies the clock, so tests are deterministic.
	Now func() time.Time
	// Backends are additional transports this engine may route effects through.
	//
	// Agent Deck is always registered as the legacy default, so a caller that
	// names none behaves exactly as before. Naming a backend here makes effects
	// for its sessions routable; it does NOT make them delivered, which is why
	// registration alone cannot silently change an outcome.
	Backends []runtime.Backend
}

// Engine owns one request's supervision.
type Engine struct {
	requestID             string
	stopTransports        func()
	transportsInitialized bool
	store                 *Store
	runtime               agentdeck.Runtime
	cfg                   *Config
	opts                  EngineOptions
	dispatch              *Dispatcher
	// backends is the same registry the dispatcher routes through, so activation
	// and dispatch cannot disagree about which transport a role runs on.
	backends map[string]runtime.Backend
	// journal is the durable observation log for this request, and ingest tracks
	// each source's continuity within it.
	journal *runtime.Journal
	ingest  *JournalIngestor
	// journalDir is remembered so the journal can be reopened after a restart.
	journalDir string
	// journalErr records why the journal could not be opened, if it could not.
	// It is kept rather than returned from the constructor so ownership is still
	// released correctly; Run refuses to start on it.
	journalErr error
	// followerDone is closed when the engine shuts down, and followerWG tracks
	// the observation followers.
	//
	// Followers belong to the ENGINE, not to one Run call: a Run may return while
	// a follower is mid-write, and a follower that outlives Close keeps writing
	// into a store its owner believes is finished. Close waits for them, which is
	// what makes "the engine is closed" mean the writes have stopped.
	followerDone chan struct{}
	followerWG   sync.WaitGroup
	closeOnce    sync.Once
	now          func() time.Time
	release      func()
	// cleanup removes a dry run's scratch tree.
	cleanup func()
	// dispatched and blocked accumulate during a run so the caller can report
	// what actually happened rather than what was intended.
	dispatched int
	blocked    int
	// lastSample is the instant active time has been charged up to.
	//
	// It is an engine-local watermark rather than persisted state: on a restart
	// there is no observation of what the worker was doing while the process was
	// gone, so that interval is unobserved and must not be charged. The watermark
	// therefore starts empty on every run, which is what makes "never charge
	// unknown downtime" structural rather than something to remember.
	lastSample time.Time
	// lastEpisodeBlock suppresses repeated diagnostic lines; it is not a phase
	// decision or durable execution state.
	lastEpisodeBlock string
}

// NewEngine builds an engine for a request and takes single ownership.
//
// Ownership is acquired here rather than at Run time, so a caller discovers a
// conflict before doing any setup work.
//
// The parameter is named rt rather than runtime so the runtime PACKAGE stays
// referable in this function, where the backend registry is built.
func NewEngine(store *Store, rt agentdeck.Runtime, requestID string, opts EngineOptions) (*Engine, error) {
	if store == nil {
		return nil, errors.New("agent-engine: engine requires a store")
	}
	if rt == nil {
		return nil, errors.New("agent-engine: engine requires an Agent Deck runtime")
	}
	if err := store.RequireNoLegacyState(requestID); err != nil {
		return nil, err
	}
	release, err := store.AcquireOwnership(requestID)
	if err != nil {
		return nil, err
	}
	// A dry run operates on a disposable copy of the request, so the real state,
	// cursor, budgets and effects are structurally out of reach rather than
	// merely unreached by the code paths we remembered to guard.
	cleanup := func() {}
	if opts.DryRun {
		scratch, done, serr := store.ScratchStore(requestID)
		if serr != nil {
			release()
			return nil, serr
		}
		store, cleanup = scratch, done
	}
	if !opts.DryRun {
		state, err := store.State(requestID)
		if err != nil {
			release()
			return nil, err
		}
		// Cancellation only stops owned work and must remain possible even when
		// another request now holds the workspace. It never launches a writer.
		if !state.Stage.Terminal() && !state.CancellationRequested {
			releaseWorkspaces, err := store.acquireEpisodeWorkspaces(state)
			if err != nil {
				release()
				return nil, err
			}
			releaseRequest := release
			release = func() { releaseWorkspaces(); releaseRequest() }
		}
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	e := &Engine{
		requestID:    requestID,
		followerDone: make(chan struct{}),
		store:        store,
		runtime:      rt,
		cfg:          store.Config(),
		opts:         opts,
		now:          now,
		release:      release,
		cleanup:      cleanup,
	}
	d := NewDispatcher(store, rt)
	d.now = now
	d.DryRun = opts.DryRun
	// Additional transports register here, so an effect is routed by the backend
	// recorded for its session rather than by whichever one the engine holds.
	for _, b := range opts.Backends {
		d.WithBackend(b)
	}
	// Aliased AFTER registration, so activation sees the same populated registry
	// the dispatcher routes through rather than a snapshot taken one line early.
	e.backends = d.backends
	e.openJournal(requestID)
	if opts.Log != nil {
		d.Log = opts.Log
	}
	e.dispatch = d
	return e, nil
}

// Close releases ownership and removes any dry-run scratch tree.
func (e *Engine) Close() {
	// Stop the observation followers FIRST, and wait for them. They write to the
	// store, so releasing ownership while one is mid-write would leave a write
	// racing the next owner of the same request.
	e.closeOnce.Do(func() {
		if e.followerDone != nil {
			close(e.followerDone)
		}
	})
	e.followerWG.Wait()

	if e.stopTransports != nil {
		e.stopTransports()
		e.stopTransports = nil
	}
	if e.release != nil {
		e.release()
		e.release = nil
	}
	if e.cleanup != nil {
		e.cleanup()
		e.cleanup = nil
	}
}

// InitializeTransports reaps previous owned processes and starts replacements
// while this engine holds request ownership. The factory must clean up partial
// startup on error. Successfully started transports stop before ownership is
// released, so another supervisor cannot race startup or shutdown.
func (e *Engine) InitializeTransports(ctx context.Context, requestID string, factory func() ([]runtime.Backend, func(), error)) error {
	if e.opts.DryRun || e.release == nil {
		return errors.New("agent-engine: transport initialization requires live request ownership")
	}
	if e.transportsInitialized {
		return errors.New("agent-engine: transports are already initialized")
	}
	if requestID != e.requestID {
		return errors.New("agent-engine: transport initialization must name the owned request")
	}
	if problems := e.ReapOrphanedProcesses(ctx, requestID); len(problems) > 0 {
		return fmt.Errorf("agent-engine: orphan quiescence not established: %s", strings.Join(problems, "; "))
	}
	state, err := e.store.State(requestID)
	if err != nil {
		return err
	}
	if state.CancellationRequested {
		// Recovery must establish quiescence, without spawning replacement
		// transports for work the operator has already cancelled.
		e.transportsInitialized = true
		return nil
	}
	backends, stop, err := factory()
	if err != nil {
		return err
	}
	e.stopTransports = stop
	e.transportsInitialized = true
	for _, backend := range backends {
		e.dispatch.WithBackend(backend)
	}
	// Record the processes this supervisor now owns, BEFORE anything else.
	//
	// Without this the pid first appears in a binding at activation time, so a
	// crash between "process started" and "binding committed" leaves an orphan
	// that nothing can find, name or reap. Recording the intent here is what
	// makes that window recoverable: the next start knows a process was ours.
	_, rerr := e.store.UpdateCommitted(requestID, func(st *State) error {
		if st.OwnedProcesses == nil {
			st.OwnedProcesses = map[string]OwnedProcess{}
		}
		for _, backend := range backends {
			pid, owns := backend.ProcessIdentity()
			if !owns || pid <= 0 {
				continue
			}
			st.OwnedProcesses[backend.Name()] = OwnedProcess{
				PID: pid, StartToken: pidStartToken(pid), RecordedAt: e.now().UTC().Format(time.RFC3339),
			}
		}
		return nil
	})
	return rerr
}

func (e *Engine) logf(format string, args ...any) {
	if e.opts.Log != nil {
		e.opts.Log(fmt.Sprintf(format, args...))
	}
}

// RunResult summarises one run.
type RunResult struct {
	// FramesObserved counts every frame seen, including ones belonging to other
	// sessions. The checkpoint advanced past all of them.
	FramesObserved int
	// FramesDecided counts frames that produced a decision.
	FramesDecided int
	// EffectsDispatched counts instructions handed to the transport.
	EffectsDispatched int
	// BlockedEffects counts instructions whose outcome could not be proven.
	BlockedEffects int
	// Stage is the stage the request ended in.
	Stage Stage
	// BoundaryReached is true when a --once run processed through its captured
	// cursor.
	BoundaryReached bool
}

// Run follows the stream and supervises the request until it is done, cancelled
// or its boundary is reached.
//
// The per-frame sequence is fixed, and the order is the design:
//
//  1. Decide from the frame, using the binding as it is now.
//  2. Commit the decision, its effect and the cursor in ONE state transaction.
//  3. Dispatch the effect immediately, while the follow is live.
//
// Step 2 is atomic because a decision whose cursor advanced but whose effect was
// never recorded — or the reverse — is silent corruption: the workflow would
// believe it had taken a step it never took, or re-decide a frame it already
// acted on.
func (e *Engine) Run(ctx context.Context, requestID string) (RunResult, error) {
	var res RunResult

	st, err := e.store.State(requestID)
	if err != nil {
		return res, err
	}
	res.Stage = st.Stage

	if st.Stage.Terminal() {
		return res, nil
	}
	// The observation log is a precondition, not a nicety. A supervisor that
	// cannot read its own history would decide from whatever happens to be on
	// the stream right now, which is the evidence-free operation this layer
	// exists to prevent.
	if e.journalErr != nil {
		return res, fmt.Errorf("agent-engine: the observation journal could not be opened, so this request "+
			"cannot be supervised: %w", e.journalErr)
	}
	// Replay first: an observation journaled before a crash whose decision was
	// never committed is replayed rather than lost.
	if _, err := e.replayUnconsumed(requestID); err != nil {
		return res, err
	}
	// Restart uncertainty is resolved before anything new is decided. An effect
	// left in flight is reconciled through the queue's own status interface.
	unresolved, err := e.dispatch.Reconcile(ctx, requestID)
	if err != nil {
		return res, fmt.Errorf("agent-engine: reconciling in-flight instructions: %w", err)
	}
	for _, eff := range unresolved {
		if eff.Status == EffectBlocked {
			res.BlockedEffects++
			e.logf("effect %s is blocked: %s", eff.EffectID, eff.LastError)
		}
	}

	health, err := e.runtime.BusHealth(ctx)
	if err != nil {
		return res, fmt.Errorf("agent-engine: checking bus health: %w", err)
	}
	if !health.OK() {
		return res, fmt.Errorf("agent-engine: the event bus is disabled; refusing to run a supervisor that would see nothing while appearing healthy")
	}

	if !e.opts.DryRun {
		if _, err := e.dispatch.Drain(ctx, requestID); err != nil {
			return res, err
		}
	}

	after := st.BusCursor
	if !st.CursorSet {
		// A request that has never consumed a frame starts at the binding
		// boundary, so events published after admission are not skipped.
		after = st.BindingBoundary
	}

	frames, errs, err := e.runtime.Events(ctx, after)
	if err != nil {
		return res, err
	}

	// Every OTHER backend is followed alongside this one, in its own units. They
	// are never merged into the frame stream above: an Agent Deck cursor and an
	// ACP connection ordinal are different units, and a combined position would
	// look like a cursor while being meaningless.
	//
	// They feed the supervision CLOCK, not the completion path. A finished ACP
	// turn is not finished work, so it may never open an audit — see
	// observe_source.go.
	marks := newObservationWatermarks()
	for _, b := range e.followedBackends() {
		e.followBackend(ctx, requestID, b, marks)
	}

	// A --once run processes through a boundary captured now. A zero boundary
	// means the stream has nothing after it, which is a legitimate empty backlog
	// and terminates immediately rather than following a stream that will never
	// produce a frame to satisfy the stop condition.
	boundary := health.Cursor
	if !e.opts.Once {
		boundary = 0
	}
	if e.opts.Once && boundary == 0 {
		e.logf("once: the stream is empty, nothing to process")
		// An empty backlog is not a reason to skip lifecycle work that does not
		// depend on the stream.
		//
		// Cancellation is durable intent: an operator recorded "stop this" and
		// nothing on the bus will ever mention it. Returning here before the
		// supervision tick would leave the request waiting forever for a frame
		// that only a live worker would produce — which is exactly what
		// cancellation is meant to stop.
		if !e.opts.DryRun {
			if err := e.DriveTaskLifecycle(ctx, requestID); err != nil {
				return res, err
			}
			// The episode tick sits beside the task tick, never inside it: a
			// request is one or the other, and this is a no-op for a task request.
			if err := e.driveEpisodeIfPresent(ctx, requestID); err != nil {
				return res, err
			}
			if sc, serr := e.store.State(requestID); serr == nil {
				res.Stage = sc.Stage
			}
		}
		return res, nil
	}
	if e.opts.Once {
		e.logf("once: processing through cursor %d", boundary)
	}

	// The engine owns a clock. Without it the loop is purely reactive, and a
	// worker that has stopped producing frames — the case that matters most — is
	// indistinguishable from a slow one. The tick is the only thing that can
	// notice that nothing has been proven for too long.
	ticker := time.NewTicker(supervisionTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return res, ctx.Err()

		case <-ticker.C:
			// Supervision runs through the same state transaction as every other
			// decision, so the tick never mutates state on its own.
			if err := e.store.Update(requestID, func(s *State) error {
				now := e.now()
				s.Supervision.LastTickAt = now.UTC().Format(time.RFC3339)
				cause, due := e.tick(s, now)
				if !due {
					return nil
				}
				policy := e.cfg.Checkpoints
				if policy.SoftActiveBudget <= 0 {
					policy = DefaultCheckpointPolicy()
				}
				wake, why := s.Supervision.ShouldWake(cause, now, policy)
				if !wake {
					e.logf("supervision suppressed a wake: %s", why)
					return nil
				}
				s.Supervision.NoteWake(cause, now)
				return e.openDiagnosticOrReview(s, cause, now)
			}); err != nil {
				return res, err
			}
			if !e.opts.DryRun {
				if _, err := e.dispatch.Drain(ctx, requestID); err != nil {
					return res, err
				}
				if err := e.DriveTaskLifecycle(ctx, requestID); err != nil {
					return res, err
				}
				// Same shape test, same independence from the task tick above.
				if err := e.driveEpisodeIfPresent(ctx, requestID); err != nil {
					return res, err
				}
			}
			if sc, err := e.store.State(requestID); err == nil {
				res.Stage = sc.Stage
				if sc.Stage.Terminal() {
					return res, nil
				}
			}

		case err, ok := <-errs:
			if err != nil {
				return res, err
			}
			if !ok {
				errs = nil
			}

		case f, ok := <-frames:
			if !ok {
				// The follower exited. That is not a finished request: the stream
				// ending says nothing about the work. It must be reconciled.
				if boundary > 0 {
					res.BoundaryReached = true
				}
				final, ferr := e.store.State(requestID)
				if ferr == nil {
					res.Stage = final.Stage
				}
				return res, fmt.Errorf("agent-engine: the event follower exited unexpectedly; the request is not finished and must be reconciled")
			}

			res.FramesObserved++
			// Journal BEFORE deciding. The record is durable whether or not the
			// decision below commits, so a crash here replays rather than loses.
			if _, ierr := e.ingestEvent(runtime.Event{
				Backend: agentdeck.BackendName, Kind: f.Kind, SessionID: f.SessionID,
				Cursor: f.Cursor, Payload: f.Data,
			}); ierr != nil {
				return res, fmt.Errorf("agent-engine: journaling observation %d: %w", f.Cursor, ierr)
			}
			// A gap means observations were lost forever. Concluding past one is
			// concluding from evidence that does not exist, so the request stops
			// rather than proceeding on a stream it can no longer trust.
			if reason := e.ingest.GapReason(); reason != "" {
				if _, berr := e.store.UpdateCommitted(requestID, func(s *State) error {
					s.StageReason = reason
					return s.transition(StageBlocked, reason, e.now().UTC().Format(time.RFC3339))
				}); berr != nil {
					return res, berr
				}
				e.logf("%s", reason)
				res.Stage = StageBlocked
				return res, fmt.Errorf("agent-engine: %s", reason)
			}
			decided, derr := e.handleFrame(ctx, requestID, f)
			if derr != nil {
				return res, derr
			}
			if decided {
				res.FramesDecided++
			}

			// The boundary is checked for every frame, including frames belonging
			// to other sessions and frames that decide nothing. Checking it only
			// after a decision would let an all-irrelevant backlog block forever.
			if boundary > 0 && f.Cursor >= boundary {
				res.BoundaryReached = true
				final, ferr := e.store.State(requestID)
				if ferr == nil {
					res.Stage = final.Stage
				}
				return res, nil
			}
		}
	}
}

// followedBackends lists the transports this run observes besides Agent Deck.
//
// Agent Deck is excluded because the run already follows it directly; including
// it here would subscribe to the same stream twice and double-charge the clock.
func (e *Engine) followedBackends() []runtime.Backend {
	var out []runtime.Backend
	for name, b := range e.backends {
		if name == agentdeck.BackendName || b == nil {
			continue
		}
		out = append(out, b)
	}
	// Stable order, so two runs follow the same transports in the same sequence.
	sort.Slice(out, func(a, b int) bool { return out[a].Name() < out[b].Name() })
	return out
}

// sampleActivity converts one observed frame into an active-time charge.
//
// The rule is deliberately conservative: time is charged only between two
// observations that both said the worker was running, and only up to the
// moment of the later observation. An interval that starts unobserved — the
// first frame after a start or a restart — is not charged at all, because
// nothing is known about what happened in it.
func (e *Engine) sampleActivity(s *State, f agentdeck.Frame, working bool) {
	at := time.UnixMilli(f.TS).UTC()
	if at.IsZero() || at.Unix() <= 0 {
		// A frame with no usable timestamp cannot bound an interval.
		return
	}
	from := e.lastSample
	e.lastSample = at
	if from.IsZero() || !at.After(from) {
		return
	}
	s.Checkpoint.Charge(ActiveWork{From: from, To: at, Paused: !working})
	if working {
		s.Checkpoint.Observe(at)
	}
}

// maybeScheduleReview opens a CHECKPOINT review when the policy says one is due.
//
// It runs inside the caller's state transaction, so opening a review and
// recording the instruction that asks for it cannot come apart. It refuses to
// open a second review while one is outstanding, because two outstanding reviews
// of the same work produce two answers and at least one of them is wasted or
// contradicts the other.
func (e *Engine) maybeScheduleReview(s *State, now time.Time) (bool, error) {
	policy := e.cfg.Checkpoints
	if policy.SoftActiveBudget <= 0 {
		policy = DefaultCheckpointPolicy()
	}
	due, reason := s.Checkpoint.Due(policy, now)
	if !due {
		return false, nil
	}
	if s.ReviewSession(ReviewCheckpoint) == "" {
		// Nothing can review this. That is a block, not a reason to keep ticking.
		s.Checkpoint.ReviewPending = true
		s.StageReason = "a checkpoint review is owed but no auditor session is bound"
		if err := s.transition(StageBlocked, s.StageReason, now.UTC().Format(time.RFC3339)); err != nil {
			return false, err
		}
		return true, nil
	}
	reviewID := fmt.Sprintf("C%d", s.Checkpoint.Sequence+1)
	s.ActiveReview = NewActiveReview(ReviewCheckpoint, reviewID, s.ReviewSession(ReviewCheckpoint), s.Generation, s.Checkpoint.Sequence+1)
	s.ActiveReview.RequestedAt = now.UTC().Format(time.RFC3339)
	s.Checkpoint.ReviewPending = true
	// The debt is not reset by scheduling. Only a judgement resets it, because
	// scheduling a review has not established that anything is going well.
	if err := s.RecordEffect(Effect{
		SessionID: s.ReviewSession(ReviewCheckpoint),
		Role:      "auditor",
		Message:   checkpointReviewPrompt(s, reason),
		Reason:    "checkpoint review scheduled: " + reason,
		CreatedAt: now.UTC().Format(time.RFC3339),
	}); err != nil {
		return false, err
	}
	if s.Stage == StageWorking {
		if err := s.transition(StageCheckpointReview, reason, now.UTC().Format(time.RFC3339)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// checkpointReviewPrompt builds a reviewer's instruction.
//
// It names the open obligation, what was last reviewed, and what is new,
// because a reviewer told only "something changed" produces an answer that
// cannot be acted on. It also asks for the negative claims to be challenged,
// since an absence assertion is the easiest kind of claim to accept uncritically.
func checkpointReviewPrompt(s *State, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A checkpoint review is owed for task %s (generation %d).\n", s.TaskID, s.Generation)
	fmt.Fprintf(&b, "Reason: %s\n\n", reason)

	b.WriteString("Open obligations:\n")
	for _, o := range s.Obligations {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", o.Disposition, o.ID, o.Criterion)
	}
	if s.Checkpoint.LastReviewedAt != "" {
		fmt.Fprintf(&b, "\nPreviously reviewed at %s, over evidence %s.\n",
			s.Checkpoint.LastReviewedAt, strings.Join(s.Checkpoint.LastReviewedEvidenceIDs, ", "))
	}
	if n := len(s.Checkpoint.Submissions); n > 0 {
		last := s.Checkpoint.Submissions[n-1]
		b.WriteString("\nMost recent worker submission:\n")
		fmt.Fprintf(&b, "  claim:      %s\n", last.Claim)
		fmt.Fprintf(&b, "  next:       %s\n", last.NextAction)
		if last.Blocker != "" {
			fmt.Fprintf(&b, "  blocker:    %s\n", last.Blocker)
		}
		for _, n := range last.NegativeClaims {
			fmt.Fprintf(&b, "  claim %s: %s is %s (recorded conclusion: %s)\n",
				n.ClaimID, n.Subject, n.Conclusion, n.Conclusion)
			if len(n.Queries) == 0 || (len(n.Alternatives) == 0 && len(n.Queries) < 2) {
				b.WriteString("    NOTE: the search behind this claim is not documented; treat it as UNKNOWN and challenge it\n")
			}
		}
	}
	b.WriteString("\nJudge against the evidence, not against activity. Commits, changed files, green commands and\n")
	b.WriteString("token counts are activity facts until linked to a criterion. Challenge any negative claim whose\n")
	b.WriteString("search is not documented. Answer with one of: CONTINUE, REDIRECT, COLLECT_EVIDENCE, DIAGNOSE,\n")
	b.WriteString("REFRESH, BLOCKED. You may not approve the request from a checkpoint review.\n")
	return b.String() + reviewAnswerInstruction(s)
}

// reviewNonce derives the correlation token for one review request.
//
// It is derived rather than random so that reconstructing a review after a
// restart produces the same nonce, and a reviewer that legitimately retries does
// not have to be told a different answer each time. The inputs are exactly the
// things that make a review distinct, so two reviews can never share one.
func reviewNonce(reviewID string, generation, attempt int) string {
	return sha256Hex([]byte(fmt.Sprintf("agent-engine/review-nonce\x00%s\x00%d\x00%d", reviewID, generation, attempt)))
}

// handleFrame commits one frame's decision, effect and cursor atomically.
func (e *Engine) handleFrame(ctx context.Context, requestID string, f agentdeck.Frame) (bool, error) {
	if f.Kind == agentdeck.KindSessionFinished {
		st, err := e.store.State(requestID)
		if err != nil {
			return false, err
		}
		if st.Episode != nil && st.Episode.Phase == string(PhaseImplement) && f.SessionID == st.WorkerSession {
			var payload struct {
				DoneStatus string `json:"done_status"`
				Model      string `json:"model"`
			}
			step, ok := st.episodeStep(episodeStepKey(PhaseImplement, st.episodePhaseRound(PhaseImplement)))
			entered, parseErr := time.Parse(time.RFC3339, step.At)
			if ok && parseErr == nil && !time.UnixMilli(f.TS).Before(entered) && f.Payload(&payload) == nil && strings.EqualFold(payload.DoneStatus, "ok") {
				if err := e.recordEpisodeTurn(requestID, agentdeck.BackendName, f.SessionID, f.EventID, "", payload.Model); err != nil {
					e.logf("episode turn refused: %v", err)
				}
			}
		}
	}

	var decided bool
	var recorded []Effect
	// anyEffect is true when the transaction recorded an effect of any kind,
	// including one opened by the review scheduler rather than by a completion.
	anyEffect := false

	err := e.store.Update(requestID, func(s *State) error {
		// The cursor advances for every frame, whether or not it concerns this
		// request. A checkpoint that only moves for interesting frames stalls
		// permanently on a busy profile.
		s.BusCursor = f.Cursor
		s.CursorSet = true
		// The JOURNAL position advances in this same transaction. It is the only
		// position comparable across backends, and advancing it separately from
		// the decision is exactly the split that replays an observation whose
		// effect was never committed.
		if e.ingest != nil {
			s.ConsumedJournalSeq = e.ingest.lastSequence()
		}

		// The binding is resolved now, not captured when the follow started, so a
		// replacement worker is visible to a live follow.
		if f.SessionID != s.WorkerSession && f.SessionID != s.AuditorSession && f.SessionID != s.CheckpointSession && f.SessionID != s.DiagnosticSession {
			return nil
		}
		if s.Stage.Terminal() {
			// Nothing may reopen a terminal request. A late event from a retired
			// worker must not undo the only conclusion that matters.
			return nil
		}
		if f.Kind != agentdeck.KindSessionFinished && f.Kind != agentdeck.KindSessionTransition {
			return nil
		}
		if f.Cursor <= s.BindingBoundary && s.BindingBoundary > 0 {
			// A frame from before the current binding belongs to a retired
			// generation and cannot drive new work.
			return nil
		}

		// A status transition is the only observation that says whether the worker
		// is executing. It drives the active-time clock, and it is deliberately not
		// a decision: a status report is not a completion, a stall, or an
		// obligation advance.
		if f.Kind == agentdeck.KindSessionTransition && f.SessionID == s.WorkerSession {
			var payload struct {
				FromStatus string `json:"from_status"`
				ToStatus   string `json:"to_status"`
			}
			if perr := f.Payload(&payload); perr == nil {
				e.sampleActivity(s, f, payload.ToStatus == "running")
			}
		}

		// A completion assertion is the only signal that may start an audit
		// request. It is a claim, never a verification.
		//
		// Only the IMPLEMENTER's finished event can open one. The reviewer's own
		// completion saying the work is done is the reviewer approving itself,
		// and admitting it would make the audit self-certifying from the first
		// frame.
		//
		// The routing itself is delegated to RequestCompletionAssessment, which
		// is the SAME function the ACP transport uses. One policy, two transports:
		// a second implementation here would be free to drift, and the drifted
		// one is the one that ships.
		if s.Episode == nil && f.Kind == agentdeck.KindSessionFinished && f.SessionID == s.WorkerSession {
			var payload struct {
				DoneStatus  string `json:"done_status"`
				DoneSummary string `json:"done_summary"`
			}
			if perr := f.Payload(&payload); perr == nil && strings.EqualFold(payload.DoneStatus, "ok") {
				// Work has ended, so the active clock stops here.
				e.sampleActivity(s, f, false)
				decided = true

				claim := CompletionClaim{
					Backend:    agentdeck.BackendName,
					SessionID:  f.SessionID,
					Generation: s.Generation,
					// EventID is the frame's own identity, which is what makes a
					// replayed history entry recognisable as the same claim.
					TurnID:  f.EventID,
					Summary: payload.DoneSummary,
				}
				if claim.TurnID == "" {
					// A finished event with no identity cannot be deduplicated, and
					// an undeduplicable claim would let a duplicate open a second
					// audit. The cursor is a weaker but stable substitute.
					claim.TurnID = fmt.Sprintf("agent-deck:%d", f.Cursor)
				}
				eff, opened, aerr := s.RequestCompletionAssessment(claim, e.now())
				if aerr != nil {
					return aerr
				}
				if opened {
					recorded = append(recorded, eff)
				}
			}
		}

		// Effects are recorded inside the same transaction as the decision.
		for _, eff := range recorded {
			if eff.SessionID == "" {
				// No recipient bound yet; recording it would create an effect that
				// can never be delivered and cannot be reconciled.
				e.logf("no %s session bound; withholding %s", eff.Role, eff.Reason)
				continue
			}
			eff.CreatedAt = s.UpdatedAt
			if err := s.RecordEffect(eff); err != nil {
				return err
			}
			anyEffect = true
		}

		// The review scheduler runs in the same transaction, so opening a review
		// and recording the instruction that requests it cannot come apart. A
		// completion review already opened above suppresses this: the request is
		// already owed, and a second one would be redundant.
		if s.ActiveReview == nil && !s.Stage.Terminal() {
			opened, err := e.maybeScheduleReview(s, e.now())
			if err != nil {
				return err
			}
			if opened && s.Stage != StageBlocked {
				anyEffect = true
			}
		}
		return nil
	})
	if err != nil {
		return decided, err
	}

	if !anyEffect || e.opts.DryRun {
		return decided, nil
	}
	// Dispatch while the follow is still live. Draining only on the way out would
	// leave a real recipient unnotified for as long as the supervisor runs.
	dr, derr := e.dispatch.Drain(ctx, requestID)
	if derr != nil {
		return decided, derr
	}
	e.dispatched += dr.Delivered
	e.blocked += dr.Blocked
	return decided, nil
}

// completionAuditPrompt builds the instruction for an independent reviewer.
func completionAuditPrompt(s *State, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An independent completion audit is owed for task %s (generation %d).\n\n", s.TaskID, s.Generation)
	if summary != "" {
		fmt.Fprintf(&b, "The worker asserted: %s\n\n", truncateForPrompt(summary, 2000))
	}
	b.WriteString("Judge the work against the request's obligations and required checks.\n")
	b.WriteString("A successful command, a commit, or a change in activity is not evidence that an obligation was met.\n")
	b.WriteString("Submit your typed answer through the control layer; do not edit machine state directly.\n")
	b.WriteString("Only a completion PASS for this request may verify it, and only while the required receipts are current.\n")
	return b.String()
}

func truncateForPrompt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[truncated]"
}

// dispatched and blocked are accumulated by handleFrame.

// RequireNoLegacyState refuses a request directory carrying legacy
// embedded-workflow state.
//
// The legacy state is deliberately refused rather than migrated. It records
// completion and audit conclusions that were never correlated to a request, a
// generation or a reviewer, and adopting them would let an approval that was
// never actually given become this engine's starting truth. A legacy request
// must be re-admitted through the control contract instead.
func (s *Store) RequireNoLegacyState(requestID string) error {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return err
	}
	// The embedded draft's state file is STATE.json inside the project, and this
	// module's is STATE.json inside a request directory. A project-local
	// .agent-workflow directory beside a request's project means the embedded
	// engine could still be running and believing it owns the same work.
	projectWorkflow := filepath.Join(filepath.Dir(s.cfg.StoreRoot), DirNameLegacy)
	if _, err := os.Stat(projectWorkflow); err == nil {
		return fmt.Errorf("%w: found %s beside the store; two engines could both claim the same work",
			ErrLegacyState, projectWorkflow)
	}
	// A legacy artifact inside the request directory itself is equally disqualifying.
	for _, name := range legacyStateNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%w: found %s in the request directory", ErrLegacyState, name)
		}
	}
	return nil
}

// DirNameLegacy is the embedded draft's workflow directory name.
const DirNameLegacy = ".agent-workflow"

// legacyStateNames are artifacts only the embedded draft produced.
var legacyStateNames = []string{"AUDIT.md", "HANDOFF.md", "STUCK_REPORT.md", "QUESTIONS.md", "DECISIONS.md"}

// --- ownership -------------------------------------------------------------

// AcquireOwnership takes the single-owner lock for one request.
//
// It is deliberately non-blocking and reports refusal rather than waiting: a
// second engine silently queueing behind the first would look healthy while
// doing nothing useful.
func (s *Store) AcquireOwnership(requestID string) (func(), error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agent-engine: creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, OwnershipLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("agent-engine: opening ownership lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrOwnershipHeld, path)
		}
		return nil, fmt.Errorf("agent-engine: taking ownership lock: %w", err)
	}
	// The owning pid is recorded so a stale lock left by a killed process can be
	// diagnosed rather than guessed at.
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// OwnershipHeld observes the existing single-owner lock without creating a
// request directory or lock file. It is a point-in-time lifecycle fact; callers
// must not treat a free lock as permission to start work without using
// AcquireOwnership.
func (s *Store) OwnershipHeld(requestID string) (bool, error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, OwnershipLockName), os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("agent-engine: reading ownership lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("agent-engine: checking ownership lock: %w", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
