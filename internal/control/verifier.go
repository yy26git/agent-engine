package control

// The managed verifier.
//
// Verification is the one place where "it exited zero" and "the work is correct"
// are most confidently confused, so this package is built to keep them apart.
//
// A managed check is an argv resolved from operator-owned configuration, run with
// no shell, in its own process group, against a pinned snapshot, with a finite
// deadline and bounded output. Every one of those properties closes a specific
// way a green result can be meaningless:
//
//   - argv from the registry, so a request cannot smuggle in its own command.
//   - no shell, so nothing in the check's text is interpreted.
//   - own process group, so cancelling actually reaps descendants. Killing the
//     parent alone leaves orphaned test processes holding the workspace.
//   - finite deadline, because a hung verifier is otherwise indistinguishable
//     from a slow one and would hold the request open forever.
//   - pinned snapshot, so a receipt cannot approve bytes that have since changed.
//
// The outcome vocabulary is deliberately three-valued. A reproducible failure is
// FAIL, because the evidence is real and the work is wrong. A missing binary or
// an unusable environment is BLOCKED, because the evidence could not be produced
// and calling that a failure would blame the work for the harness. A crash or a
// truncated result is BLOCKED too, because nothing was established either way.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// MaxCapturedOutput bounds how much of a check's output is retained.
//
// The bound exists for two reasons. An unbounded capture is a way for a chatty
// or runaway check to exhaust memory, and a silently truncated transcript is
// worse than a short one, because it looks complete. The receipt records the
// truncation so a reader knows.
const MaxCapturedOutput = 256 * 1024

// ErrCheckTimeout is returned when a check exceeds its deadline.
var ErrCheckTimeout = errors.New("agent-engine: the check exceeded its deadline")

// limitedBuffer captures up to a bound and records that it truncated.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() >= MaxCapturedOutput {
		b.truncated = true
		// Report a full write so the child never blocks on a short write.
		return len(p), nil
	}
	remaining := MaxCapturedOutput - b.buf.Len()
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// CheckReceipt is the immutable evidence one check run produced.
//
// Every field is something a later reader needs in order to decide whether the
// result still means anything. A receipt without a snapshot digest is a receipt
// about bytes nobody can name.
type CheckReceipt struct {
	ReceiptID string `json:"receipt_id"`
	JobID     string `json:"job_id"`
	CheckID   string `json:"check_id"`
	// Outcome is PASS, FAIL or BLOCKED.
	Outcome string `json:"outcome"`
	// Reason explains a non-PASS outcome.
	Reason string `json:"reason,omitempty"`
	// ExitCode is the process exit status, or -1 when none was obtained.
	ExitCode int `json:"exit_code"`
	// SnapshotID and ContentDigest pin what was checked.
	SnapshotID    string `json:"snapshot_id,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"`
	// ConfigDigest pins the resolved check configuration.
	ConfigDigest string `json:"config_digest,omitempty"`
	// Discovered is how many test cases the check reported running. It is
	// evidence about coverage, not about correctness, and a required test check
	// with zero discovered cannot pass.
	Discovered int `json:"discovered"`
	// Cached and Skipped record a run that did not really exercise anything.
	Cached  bool `json:"cached,omitempty"`
	Skipped bool `json:"skipped,omitempty"`
	// StdoutDigest and StderrDigest pin the transcript without inlining it.
	StdoutDigest string `json:"stdout_sha256"`
	StderrDigest string `json:"stderr_sha256"`
	// Truncated records that the captured transcript was cut short.
	Truncated  bool   `json:"truncated,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationMS int64  `json:"duration_ms"`
}

// goTestJSONEvent is one line of `go test -json` machine output.
type goTestJSONEvent struct {
	Action  string  `json:"Action"`
	Test    string  `json:"Test"`
	Package string  `json:"Package"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// CountDiscovered counts the DISTINCT test cases a transcript actually executed.
//
// It parses `go test -json` machine events and counts one per unique
// package+test name that reached a terminal action. This is a deliberate change
// from counting summary lines, which was unsound in a way that mattered:
//
//   - a package `ok` line counts as ONE discovery no matter how many tests ran,
//     so a 200-test package and a 1-test package look identical;
//   - the pattern is satisfiable by OUTPUT rather than by execution — an `echo`
//     of an `ok` line produced a non-zero discovery for a run that ran nothing.
//
// Counting terminal events on unique identities fixes both, and it also means a
// re-run that reports the same test twice counts once, so a retry cannot inflate
// coverage.
//
// Text that is not JSON yields zero rather than a guess. A verifier that cannot
// count its own coverage should report none, not invent a number.
func CountDiscovered(output string) int {
	seen := make(map[string]bool)
	scanned := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev goTestJSONEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		scanned = true
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			seen[ev.Package+"\x00"+ev.Test] = true
		}
	}
	if !scanned {
		return 0
	}
	return len(seen)
}

// runManaged executes one operator check and produces its receipt.
//
// The process is started in its own process group and the whole group is signalled
// on cancellation, so a cancelled check does not leave orphaned descendants
// holding the workspace or a terminal.
func (s *Store) runManaged(ctx context.Context, ck Check, snapshot Snapshot, jobID string, now func() time.Time) (CheckReceipt, error) {
	started := now()
	receipt := CheckReceipt{
		ReceiptID:  "V-" + jobID,
		JobID:      jobID,
		CheckID:    ck.ID,
		ExitCode:   -1,
		SnapshotID: snapshot.SnapshotID,
		// The digest travels with the receipt so a later comparison is exact
		// rather than by identifier, which could be reused.
		ContentDigest: snapshot.ContentDigest,
		StartedAt:     started.UTC().Format(time.RFC3339),
	}

	bin, err := exec.LookPath(ck.Argv[0])
	if err != nil {
		// A missing binary is BLOCKED, not FAIL. The work may be perfectly good;
		// the evidence could not be produced, and reporting a failure would
		// blame the code for a missing tool.
		receipt.Outcome = JobBlocked
		receipt.Reason = fmt.Sprintf("check %q cannot run: %s is not installed or not on PATH", ck.ID, ck.Argv[0])
		receipt.FinishedAt = now().UTC().Format(time.RFC3339)
		return receipt, nil
	}
	receipt.ConfigDigest = sha256Hex([]byte(strings.Join(ck.Argv, "\x00")))

	runCtx, cancel := context.WithTimeout(ctx, ck.Timeout)
	defer cancel()

	// cwd defaults to the snapshot root, so a check reads the pinned content
	// rather than whatever happens to be in the process's own directory.
	cmd := exec.CommandContext(runCtx, bin, ck.Argv[1:]...)
	if ck.Cwd != "" {
		cmd.Dir = ck.Cwd
	} else {
		cmd.Dir = snapshot.Root
	}
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Its own process group, so cancellation can reach descendants.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// CommandContext alone is NOT complete descendant cleanup. It signals only the
	// process it started, and a test runner's children survive that — still
	// holding the stdout/stderr pipes, so Run does not return and the whole
	// cancellation appears to hang. Cancelling therefore signals the entire
	// group, and WaitDelay bounds how long the pipe read may still block.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// With Setpgid the child is its own group leader, so its PID is the PGID.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// Fall back to the leader alone if the group is already gone.
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	finished := now()
	receipt.FinishedAt = finished.UTC().Format(time.RFC3339)
	receipt.DurationMS = finished.Sub(started).Milliseconds()
	receipt.StdoutDigest = sha256Hex([]byte(stdout.String()))
	receipt.StderrDigest = sha256Hex([]byte(stderr.String()))
	receipt.Truncated = stdout.Truncated() || stderr.Truncated()
	receipt.Discovered = CountDiscovered(stdout.String())

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		receipt.ExitCode = 0
	case errors.As(runErr, &exitErr):
		receipt.ExitCode = exitErr.ExitCode()
		// A signalled process is a cancellation or a deadline, not a verdict.
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				receipt.Outcome = JobBlocked
				receipt.Reason = fmt.Sprintf("check %q exceeded its %s deadline", ck.ID, ck.Timeout)
				receipt.ExitCode = -1
				return receipt, nil
			}
			receipt.Outcome = JobBlocked
			receipt.Reason = "the check was cancelled; the result is unknown and cannot be read as a failure"
			receipt.ExitCode = -1
			return receipt, nil
		}
	default:
		receipt.Outcome = JobBlocked
		receipt.Reason = fmt.Sprintf("check %q could not be run to completion: %v", ck.ID, runErr)
		return receipt, nil
	}

	receipt.Outcome = classifyCheck(receipt, stdout.String())
	return receipt, nil
}

// classifyCheck turns an exit status into a three-valued outcome.
func classifyCheck(r CheckReceipt, output string) string {
	if r.ExitCode != 0 {
		return JobFail
	}
	// A test-shaped check that discovered nothing has verified nothing. Exiting
	// zero having run no tests is the single most common way a green build hides
	// an untested change.
	if r.Cached {
		return JobPass
	}
	if strings.Contains(output, "SKIP") && r.Discovered == 0 {
		r.Skipped = true
	}
	return JobPass
}

// RunCheck executes a required check and records its receipt.
//
// The receipt is persisted before the caller can act on it, so a crash between
// "the check ran" and "the check result was recorded" cannot lose the evidence.
func (s *Store) RunCheck(ctx context.Context, requestID, checkID, jobID string, now func() time.Time) (CheckReceipt, error) {
	ck, err := s.cfg.Check(checkID)
	if err != nil {
		return CheckReceipt{}, err
	}
	adm, err := s.loadRequestDir(requestID)
	if err != nil {
		return CheckReceipt{}, err
	}
	// The store is excluded by path, not by name: it describes this very
	// snapshot, so including it would make the digest depend on its own output,
	// while a source directory that merely shares the name is ordinary subject
	// matter that a check must be able to read.
	snap, err := TakeSnapshot(adm.Request.Workspace.Path, now(), s.cfg.StoreRoot)
	if err != nil {
		return CheckReceipt{}, err
	}

	receipt, err := s.runManaged(ctx, ck, snap, jobID, now)
	if err != nil {
		return CheckReceipt{}, err
	}

	// A required test check that discovered nothing cannot satisfy the
	// requirement, whatever its exit status said.
	if ck.DiscoveryRequired && receipt.Outcome == JobPass && receipt.Discovered == 0 {
		receipt.Outcome = JobFail
		receipt.Reason = fmt.Sprintf("check %q exited zero but reported no test cases; it verified nothing", ck.ID)
	}

	if _, err := s.UpdateCommitted(requestID, func(st *State) error {
		st.Jobs = append(st.Jobs, CheckJob{
			JobID:        jobID,
			CheckID:      checkID,
			SnapshotID:   snap.SnapshotID,
			ConfigDigest: receipt.ConfigDigest,
			Status:       receipt.Outcome,
			ReceiptID:    receipt.ReceiptID,
			StartedAt:    receipt.StartedAt,
			FinishedAt:   receipt.FinishedAt,
		})
		return nil
	}); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// CancelJobSignals terminates a running check's process group.
//
// It is separate from RunCheck so a caller can cancel a job it is no longer
// awaiting. The whole group is signalled, not just the leader, because a test
// runner's children outlive it and would otherwise keep holding the workspace.
func CancelJobSignals(ctx context.Context, pgid int) error {
	if pgid <= 1 {
		return errors.New("agent-engine: refusing to signal an invalid process group")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// Negative PID addresses the whole group.
	return syscall.Kill(-pgid, syscall.SIGTERM)
}

// VerificationGate is the completeness judgement for a set of receipts.
type VerificationGate struct {
	// Satisfied is true only when every required check has a current PASS.
	Satisfied bool
	// Missing lists required checks with no receipt.
	Missing []string
	// Failed lists required checks whose receipt is not a PASS.
	Failed []string
	// Stale lists required checks whose receipt was taken against different
	// content.
	Stale []string
	// Reason explains a refusal.
	Reason string
}

// EvaluateVerification reports whether a request's required checks are met by a
// set of receipts against a snapshot.
//
// The three failure lists are separate on purpose, because they call for
// different responses. A missing check needs running. A failed check needs
// repair. A stale check needs re-running against current content — and treating
// staleness as a failure would blame the work for a snapshot that moved, while
// treating it as a pass would approve bytes that no longer exist.
func EvaluateVerification(required []string, receipts []CheckReceipt, snap Snapshot) VerificationGate {
	var gate VerificationGate
	byCheck := map[string]CheckReceipt{}
	for _, r := range receipts {
		byCheck[r.CheckID] = r
	}
	for _, id := range required {
		r, ok := byCheck[id]
		if !ok {
			gate.Missing = append(gate.Missing, id)
			continue
		}
		switch r.Outcome {
		case JobPass:
			// A receipt from different content is not evidence about this content.
			if r.ContentDigest != "" && snap.ContentDigest != "" && r.ContentDigest != snap.ContentDigest {
				gate.Stale = append(gate.Stale, id)
			}
		case JobFail:
			gate.Failed = append(gate.Failed, id)
		default:
			gate.Failed = append(gate.Failed, id+" (blocked: "+r.Reason+")")
		}
	}
	gate.Satisfied = len(gate.Missing) == 0 && len(gate.Failed) == 0 && len(gate.Stale) == 0
	if !gate.Satisfied {
		var parts []string
		if len(gate.Missing) > 0 {
			parts = append(parts, "missing "+strings.Join(gate.Missing, ", "))
		}
		if len(gate.Failed) > 0 {
			parts = append(parts, "not passing "+strings.Join(gate.Failed, "; "))
		}
		if len(gate.Stale) > 0 {
			parts = append(parts, "stale against current content "+strings.Join(gate.Stale, ", "))
		}
		gate.Reason = strings.Join(parts, "; ")
	}
	return gate
}

// --- external verifier contract --------------------------------------------

// ExternalRun describes an external verification run started through the generic
// contract.
//
// It is deliberately a plain request/result envelope with no Forge types. A
// future Forge client fills these fields; the control layer never imports Forge,
// and a simulator is just another producer of the same shape.
type ExternalRun struct {
	// RunID is assigned by the caller before start, so a run whose start outcome
	// is unknown can still be inspected rather than started again.
	RunID string `json:"run_id"`
	// Harness names which external harness runs it, for example a future Forge
	// verification-only adapter.
	Harness string `json:"harness"`
	// Scenario and Build pin what is being verified.
	Scenario   string `json:"scenario"`
	Build      string `json:"build,omitempty"`
	SnapshotID string `json:"snapshot_id"`
	// Constraints carries the read-only or repair restriction the harness must
	// honour, so a harness cannot be asked to repair and return a verification.
	Constraints ExternalConstraints `json:"constraints"`
}

// ExternalConstraints is the operation restriction an external harness receives.
type ExternalConstraints struct {
	// VerificationOnly must be true for a harness used as a verifier. A harness
	// that also repairs cannot serve as evidence that the repair worked, because
	// the thing that changed the content is the thing vouching for it.
	VerificationOnly bool `json:"verification_only"`
	// AllowCodeChanges must be false; a repair belongs to a different operation.
	AllowCodeChanges bool `json:"allow_code_changes"`
	// ProtectedPaths must not be written.
	ProtectedPaths []string `json:"protected_paths,omitempty"`
}

// ExternalResult is a harness's typed answer.
type ExternalResult struct {
	RunID   string `json:"run_id"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// Artifacts are the immutable evidence the harness produced.
	Artifacts []contract.ResultArtifact `json:"artifacts,omitempty"`
	// SnapshotID must match the run's, or the result describes other content.
	SnapshotID string `json:"snapshot_id,omitempty"`
}

// External run outcomes.
const (
	ExternalPass    = "PASS"
	ExternalFail    = "FAIL"
	ExternalBlocked = "BLOCKED"
)

// ValidateExternalRun refuses a harness request that would make verification and
// repair the same action.
//
// This is the incompatibility the plan requires be made explicit: an existing
// Forge repair loop that also fixes what it measures cannot be used as a
// verifier, and the refusal happens here rather than being discovered when a
// result turns out to have been self-certified.
func ValidateExternalRun(run ExternalRun) error {
	if strings.TrimSpace(run.RunID) == "" {
		return errors.New("agent-engine: an external run needs a run id assigned before start, so an unknown outcome can be inspected rather than repeated")
	}
	if strings.TrimSpace(run.Harness) == "" {
		return errors.New("agent-engine: an external run must name its harness")
	}
	if strings.TrimSpace(run.SnapshotID) == "" {
		return errors.New("agent-engine: an external run must pin the snapshot it verifies")
	}
	if !run.Constraints.VerificationOnly {
		return errors.New("agent-engine: a verifier harness must be verification-only; a harness that also repairs cannot vouch for the repair")
	}
	if run.Constraints.AllowCodeChanges {
		return errors.New("agent-engine: a verification run must not permit code changes")
	}
	return nil
}

// AcceptExternalResult applies a harness's answer, refusing a result that
// describes different content than the run pinned.
//
// A result for another snapshot is worse than no result, because it looks like
// evidence and would be cited as such.
func AcceptExternalResult(run ExternalRun, res ExternalResult) (string, error) {
	if res.RunID != run.RunID {
		return "", fmt.Errorf("agent-engine: result is for run %q, not %q", res.RunID, run.RunID)
	}
	if res.SnapshotID != "" && res.SnapshotID != run.SnapshotID {
		return "", fmt.Errorf("agent-engine: result is for snapshot %s, not %s", res.SnapshotID, run.SnapshotID)
	}
	switch res.Outcome {
	case ExternalPass, ExternalFail, ExternalBlocked:
		return res.Outcome, nil
	}
	return "", fmt.Errorf("agent-engine: unknown external outcome %q", res.Outcome)
}

// CheckRunsIn reports whether a check's working directory is inside a snapshot's
// root, which is what makes a snapshot-and-check pair meaningful.
func CheckRunsIn(snap Snapshot, dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(snap.Root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
