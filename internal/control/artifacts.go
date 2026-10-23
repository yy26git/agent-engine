package control

// The artifact and result commit protocol.
//
// A terminal result is the one thing a caller will read long after the process
// that produced it has gone. So its publication is an ordered, recoverable
// sequence rather than a single write, and the ordering is the entire design:
//
//  1. The structured result body is committed to REPORTS/<result-id>.json, under
//     an immutable name, and is never edited in place.
//  2. State is committed terminal, carrying that body's SHA-256. State is the
//     commit authority: until it says terminal, nothing is finished.
//  3. RESULT.json is projected from the committed body.
//
// A crash between 2 and 3 leaves a finished request whose projection is simply
// missing, and `result` regenerates it. A crash between 1 and 2 leaves a body no
// state references, which is an orphan: reported, never accepted. A body whose
// bytes no longer match the digest state recorded is refused outright rather
// than reported with a caveat, because a caller cannot act on a result it is
// merely being warned about.
//
// This is explicitly not a claim of a multi-file atomic transaction. The
// guarantee is that the state file is the sole authority and that a result is
// only ever trusted through a reference state itself made.
//
// Relocated from the Agent Deck draft's internal/workflow/transaction.go
// (CommitReport, ReadReport, VerifyReport, OrphanReports, PruneReports), with
// the same properties and the same reasons.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// ErrReportIntegrity is returned when a committed body no longer matches the
// digest state recorded for it.
var ErrReportIntegrity = errors.New("agent-engine: committed result does not match its recorded digest")

// ErrOrphan is returned when a body exists that no state entry references.
var ErrOrphan = errors.New("agent-engine: result file is not referenced by current state")

// resultID is the immutable name of a request's result body.
func resultID(requestID string) string { return requestID + "-result" }

func reportRel(requestID, name string) string {
	return filepath.Join(RequestsDirName, requestID, ReportsDirName, name)
}

// PublishResult commits a terminal result.
//
// It requires that the state is not already terminal with a different outcome.
// Once published, a result's identity and outcome do not change; a later
// correction is a new request, because a caller holding a reference to the old
// one has no way to learn it moved.
//
// Returns the result digest.
func (s *Store) PublishResult(requestID string, res contract.Result) (string, error) {
	if !res.Status.Terminal() {
		return "", fmt.Errorf("agent-engine: %q is not a terminal status, so there is no result to publish", res.Status)
	}
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return "", err
	}
	unlock, err := s.storeLock()
	if err != nil {
		return "", err
	}
	defer unlock()

	st, err := s.loadState(dir)
	if err != nil {
		return "", err
	}
	if st.Stage.Terminal() {
		if st.ResultStatus == string(res.Status) {
			// Already published with the same outcome. Idempotent, because a
			// caller retrying a publish must not be told it failed.
			return st.ResultDigest, s.regenerateProjection(st)
		}
		return "", fmt.Errorf("agent-engine: request %s is already terminal as %s; a published result never changes, submit a new request instead",
			requestID, st.ResultStatus)
	}

	// Publication and cancellation serialize on the same state lock.
	if res.Status == contract.StatusComplete && st.CancellationRequested {
		res.Status = contract.StatusCancelled
		res.Summary = "Report cancelled before completion publication."
	}
	if res.Status == contract.StatusComplete && (st.Episode == nil || !st.Episode.ReportOnly || st.Episode.ReportOutput == nil) {
		return "", fmt.Errorf("COMPLETE requires retained report-only output")
	}
	res.Version = contract.Version
	res.RequestID = requestID
	res.RequestSHA256 = st.RequestDigest
	res.ExecutionAssurance = st.ExecutionAssurance
	res.TaskID = st.TaskID
	res.Ready = true
	res.Stage = ""
	res.FinishedAt = s.nowRFC3339()

	rel := reportRel(requestID, resultID(requestID)+".json")
	if err := writeJSONDurable(s.cfg.StoreRoot, rel, res, 0o444); err != nil {
		return "", err
	}
	digest, err := DigestFile(filepath.Join(s.cfg.StoreRoot, rel))
	if err != nil {
		return "", err
	}

	// State is the authority. Only after it commits is anything published.
	st.Stage = stageForStatus(res.Status)
	st.StageReason = res.Summary
	st.ResultDigest = digest
	st.ResultStatus = string(res.Status)
	st.History = appendHistory(st, st.Stage, "result_published",
		fmt.Sprintf("%s for request %s", res.Status, requestID))
	if err := s.saveState(dir, st); err != nil {
		return "", err
	}

	// The projection may fail without affecting the accepted decision.
	_ = s.regenerateProjection(st)
	return digest, nil
}

// writeResultBody writes a terminal result body under its immutable name.
//
// It is separated from the state commit so the body can be durable BEFORE
// anything claims it. That ordering is what makes an approval recoverable: a
// crash after the body exists leaves a state that still owes the review, and a
// resubmitted answer re-derives the same body rather than discovering a
// half-published outcome it cannot finish.
func (s *Store) writeResultBody(requestID string, res contract.Result) error {
	if !res.Status.Terminal() {
		return fmt.Errorf("agent-engine: %q is not a terminal status, so there is no result to write", res.Status)
	}
	res.Version = contract.Version
	res.RequestID = requestID
	res.Ready = true
	res.Stage = ""
	res.FinishedAt = s.nowRFC3339()
	rel := reportRel(requestID, resultID(requestID)+".json")
	return writeJSONDurable(s.cfg.StoreRoot, rel, res, 0o444)
}

// digestResultBody measures the committed body.
func (s *Store) digestResultBody(requestID string) (string, error) {
	rel := reportRel(requestID, resultID(requestID)+".json")
	return DigestFile(filepath.Join(s.cfg.StoreRoot, rel))
}

// runRequiredChecks executes every required check and returns their receipts.
//
// This is the production caller RunCheck never had. The point is not merely to
// run the checks but to run them against the content as it stands at approval
// time: each check takes its own snapshot, so a receipt is always about
// identifiable bytes.
//
// A request with no required checks yields no receipts and no error. That is a
// legitimate configuration, not a silent pass, and the caller decides whether an
// empty required set is acceptable for what it is approving.
func (s *Store) runRequiredChecks(requestID string, required []string, now func() time.Time) ([]CheckReceipt, error) {
	if len(required) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), requiredCheckBudget)
	defer cancel()

	var receipts []CheckReceipt
	for _, checkID := range required {
		// The intent is durable before the process starts, so a check whose
		// outcome is unknown after a crash is visible as an interrupted job
		// rather than as a check that was never attempted.
		var jobID string
		if _, err := s.UpdateCommitted(requestID, func(st *State) error {
			// Allocate under the state transaction so a retry/another round cannot
			// reuse the earlier failure's receipt identity.
			jobID = fmt.Sprintf("J-%s-r%d", checkID, st.Revision)
			st.Jobs = append(st.Jobs, CheckJob{
				JobID:     jobID,
				CheckID:   checkID,
				Status:    JobRunning,
				StartedAt: now().UTC().Format(time.RFC3339),
			})
			return nil
		}); err != nil {
			return nil, err
		}

		receipt, err := s.RunCheck(ctx, requestID, checkID, jobID, now)
		if err != nil {
			return nil, fmt.Errorf("agent-engine: required check %s could not be run: %w", checkID, err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

// requiredCheckBudget bounds the whole required-check sequence for one approval.
//
// It is generous because these are real builds and test suites, and finite
// because an approval that waits forever is indistinguishable from one that is
// stuck.
const requiredCheckBudget = 30 * time.Minute

// stageForStatus maps a terminal status onto its stage.
func stageForStatus(status contract.Status) Stage {
	switch status {
	case contract.StatusComplete:
		return StageReportComplete
	case contract.StatusPass:
		return StageVerified
	case contract.StatusFail:
		return StageFailed
	case contract.StatusBlocked:
		return StageBlocked
	case contract.StatusCancelled:
		return StageCancelled
	}
	return StageBlocked
}

func appendHistory(st *State, to Stage, event, note string) []HistoryEntry {
	out := append(st.History, HistoryEntry{
		At:       st.UpdatedAt,
		From:     st.Stage,
		To:       to,
		Event:    event,
		Note:     note,
		Revision: st.Revision,
	})
	if len(out) > MaxHistory {
		out = out[len(out)-MaxHistory:]
	}
	return out
}

// regenerateProjection rewrites RESULT.json from the committed body.
//
// This is what makes a crash between the state commit and the projection
// recoverable: the authoritative bytes already exist, so the missing file is
// re-derived rather than re-decided.
func (s *Store) regenerateProjection(st *State) error {
	if st.ResultDigest == "" {
		return nil
	}
	rel := reportRel(st.RequestID, resultID(st.RequestID)+".json")
	body, err := os.ReadFile(filepath.Join(s.cfg.StoreRoot, rel))
	if err != nil {
		return fmt.Errorf("agent-engine: reading committed result for %s: %w", st.RequestID, err)
	}
	if got := sha256Hex(body); got != st.ResultDigest {
		return fmt.Errorf("%w: result for %s has digest %s, state recorded %s",
			ErrReportIntegrity, st.RequestID, got, st.ResultDigest)
	}
	return writeDurable(s.cfg.StoreRoot,
		filepath.Join(RequestsDirName, st.RequestID, ResultFileName), body, 0o444)
}

// Result returns the caller-facing result for a request.
//
// A nonterminal request gets an explicit not-ready answer carrying its stage. It
// never gets a fabricated or partial result object, because a caller that
// mistakes a nonterminal stage for a finished request is the most damaging way
// this contract could lie.
func (s *Store) Result(requestID string) (*contract.Result, error) {
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	st, err := s.loadState(dir)
	if err != nil {
		return nil, err
	}
	if st.ResultDigest == "" {
		return &contract.Result{
			Version:       contract.Version,
			RequestID:     requestID,
			RequestSHA256: st.RequestDigest,
			TaskID:        st.TaskID,
			Ready:         false,
			Stage:         string(st.Stage),
			Reason:        st.StageReason,
		}, nil
	}

	rel := reportRel(requestID, resultID(requestID)+".json")
	body, err := os.ReadFile(filepath.Join(s.cfg.StoreRoot, rel))
	if err != nil {
		// The state references evidence that is gone. The honest answer is that
		// the result cannot be produced, not a result reconstructed from memory.
		return nil, fmt.Errorf("%w: result for %s is referenced by state but its body is missing: %v",
			ErrCorrupt, requestID, err)
	}
	if got := sha256Hex(body); got != st.ResultDigest {
		return nil, fmt.Errorf("%w: result for %s has digest %s, state recorded %s",
			ErrReportIntegrity, requestID, got, st.ResultDigest)
	}

	var res contract.Result
	if err := readJSONStrict(filepath.Join(s.cfg.StoreRoot, rel), &res); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}

	// Repair a projection that is missing or was damaged. This is re-derivation
	// from the committed body, never a re-decision.
	projPath := filepath.Join(dir, ResultFileName)
	if _, statErr := os.Stat(projPath); statErr != nil {
		if err := writeDurable(s.cfg.StoreRoot,
			filepath.Join(RequestsDirName, requestID, ResultFileName), body, 0o444); err != nil {
			return nil, err
		}
	}
	return &res, nil
}

// CommitReport stores an immutable report body and references it from state.
//
// Reports carry the reasoning behind a decision. They are written once under the
// request that caused them and are never edited, so a reader can trust a digest
// to mean the reasoning did not change underneath them.
func (s *Store) CommitReport(requestID, reportID string, body []byte, mutate func(*State, string) error) (string, error) {
	if err := contract.ValidateRequestID(reportID); err != nil {
		return "", fmt.Errorf("agent-engine: report id: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", fmt.Errorf("agent-engine: report %q has an empty body", reportID)
	}
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return "", err
	}
	unlock, err := s.storeLock()
	if err != nil {
		return "", err
	}
	defer unlock()

	st, err := s.loadState(dir)
	if err != nil {
		return "", err
	}
	rel := reportRel(requestID, reportID+".md")
	if err := writeDurable(s.cfg.StoreRoot, rel, body, 0o444); err != nil {
		return "", err
	}
	digest := sha256Hex(body)
	if err := mutate(st, digest); err != nil {
		return "", err
	}
	if err := s.saveState(dir, st); err != nil {
		return "", err
	}
	return digest, nil
}

// ReadReport returns a committed report body after verifying its digest.
func (s *Store) ReadReport(requestID, reportID, wantDigest string) (string, error) {
	rel := reportRel(requestID, reportID+".md")
	body, err := os.ReadFile(filepath.Join(s.cfg.StoreRoot, rel))
	if err != nil {
		return "", fmt.Errorf("agent-engine: reading report %s for %s: %w", reportID, requestID, err)
	}
	if wantDigest != "" {
		if got := sha256Hex(body); got != wantDigest {
			return "", fmt.Errorf("%w: report %s has digest %s, expected %s",
				ErrReportIntegrity, reportID, got, wantDigest)
		}
	}
	return string(body), nil
}

// OrphanResults lists result bodies no state references.
//
// These are expected crash residue: a body written just before a state commit
// that never happened. They are reported rather than silently deleted so an
// operator can see a decision was in flight, and reclaimed only explicitly.
func (s *Store) OrphanResults() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.cfg.StoreRoot, RequestsDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("agent-engine: reading requests dir: %w", err)
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		st, err := s.State(id)
		if err != nil {
			if errors.Is(err, ErrNoState) {
				orphans = append(orphans, filepath.Join(RequestsDirName, id))
			}
			continue
		}
		if st.ResultDigest == "" {
			rel := reportRel(id, resultID(id)+".json")
			if _, err := os.Stat(filepath.Join(s.cfg.StoreRoot, rel)); err == nil {
				orphans = append(orphans, rel)
			}
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// EpisodeStates returns a bounded, newest-first snapshot for local observers.
// It is a reconstructible projection over authoritative request states; callers
// still apply their own caller/template scope before presenting each row.
func (s *Store) EpisodeStates(callerPrefix string, templates map[string]bool, limit int) ([]*State, error) {
	if callerPrefix == "" || limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("episode snapshot requires a caller namespace and a limit between 1 and 100")
	}
	entries, err := os.ReadDir(filepath.Join(s.cfg.StoreRoot, RequestsDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("agent-engine: reading requests dir: %w", err)
	}
	type candidate struct {
		id      string
		modTime time.Time
	}
	candidates := make([]candidate, 0, min(limit, len(entries)))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), callerPrefix) {
			continue
		}
		info, err := os.Stat(filepath.Join(s.cfg.StoreRoot, RequestsDirName, entry.Name(), StateFileName))
		if err != nil {
			if errors.Is(err, ErrNoState) {
				continue
			}
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("agent-engine: locating episode %s: %w", entry.Name(), err)
		}
		candidates = append(candidates, candidate{id: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].id > candidates[j].id
		}
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	// The service observes at most 1000 recent candidate states per refresh,
	// regardless of how large the operator's retained request history grows.
	if len(candidates) > 1000 {
		candidates = candidates[:1000]
	}
	rows := make([]*State, 0, min(limit, len(candidates)))
	for _, candidate := range candidates {
		state, err := s.State(candidate.id)
		if err != nil {
			if errors.Is(err, ErrNoState) {
				continue
			}
			return nil, fmt.Errorf("agent-engine: reading episode %s: %w", candidate.id, err)
		}
		if state.Episode != nil {
			if len(templates) > 0 && !templates[state.EpisodeTemplate] {
				continue
			}
			request, err := s.Request(state.RequestID)
			if err != nil || !strings.HasPrefix(request.Origin.CallerID, callerPrefix) {
				continue
			}
			rows = append(rows, state)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UpdatedAt == rows[j].UpdatedAt {
			return rows[i].RequestID > rows[j].RequestID
		}
		return rows[i].UpdatedAt > rows[j].UpdatedAt
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}
