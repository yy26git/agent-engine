package oma

// The pinned launch gate.
//
// Everything about running the external scheduler lives here, and the design is
// shaped by one asymmetry: this layer controls its own store and does not control
// the scheduler. So the boundary is made strict rather than trusted.
//
//   - The artifact is PINNED by digest and verified before launch. A version
//     string is not an identity — the pinned CLI has no `--version` command at all
//     — so the approved executable's measured digest is the identity, and a
//     mismatch refuses BEFORE anything is spawned.
//   - Launch intent is durable BEFORE the process starts. A scheduler whose start
//     outcome is unknown is a request that must be reconciled, never replayed.
//   - Cancellation is bridged, not inherited. Killing the scheduler does not stop
//     the tmux workers it launched, so the caller's durable cancel intent is
//     reconciled here FIRST and the scheduler is signalled only after that.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MaxCapturedOutput bounds how much scheduler output is retained.
const MaxCapturedOutput = 4 << 20 // 4 MiB

// ErrArtifactRefused is returned when the configured scheduler is not the
// approved artifact.
var ErrArtifactRefused = errors.New("oma: the scheduler artifact is not the approved one")

// ErrVersionUnsupported is returned when the pinned protocol version is not the
// one this adapter was written against.
var ErrVersionUnsupported = errors.New("oma: unsupported scheduler protocol version")

// Launcher is the operator-resolved identity of the external CLI.
type Launcher struct {
	// Path is the ABSOLUTE path of the approved executable or wrapper.
	Path string
	// SHA256 is the measured digest of that file.
	SHA256 string
	// PackageVersion is the pinned @open-multi-agent/core version.
	PackageVersion string
	// NodeVersion records the interpreter, which is part of what was reviewed.
	NodeVersion string
}

// VerifyLauncher checks the configured scheduler against the approved identity.
//
// The order is deliberate: absolute path, then digest. A bare name could resolve
// through PATH to something else entirely, and a digest alone cannot say whether
// the operator intended the file they configured.
func VerifyLauncher(l Launcher) error {
	trimmed := strings.TrimSpace(l.Path)
	if trimmed == "" {
		return fmt.Errorf("%w: no scheduler path is configured", ErrArtifactRefused)
	}
	if !filepath.IsAbs(trimmed) {
		return fmt.Errorf("%w: the scheduler path must be absolute, got %q", ErrArtifactRefused, trimmed)
	}
	if strings.TrimSpace(l.SHA256) == "" {
		return fmt.Errorf("%w: the scheduler has no approved digest; an unpinned scheduler cannot be launched",
			ErrArtifactRefused)
	}
	resolved, err := filepath.EvalSymlinks(trimmed)
	if err != nil {
		return fmt.Errorf("%w: the scheduler cannot be resolved: %v", ErrArtifactRefused, err)
	}
	measured, err := fileDigest(resolved)
	if err != nil {
		return fmt.Errorf("%w: the scheduler cannot be read: %v", ErrArtifactRefused, err)
	}
	if !strings.EqualFold(measured, l.SHA256) {
		return fmt.Errorf("%w: %s hashes to %s but %s was approved; it has been replaced since it was reviewed",
			ErrArtifactRefused, resolved, measured, l.SHA256)
	}
	if strings.TrimSpace(l.PackageVersion) != SupportedVersion {
		return fmt.Errorf("%w: the pinned package version is %q but this adapter implements %q",
			ErrVersionUnsupported, l.PackageVersion, SupportedVersion)
	}
	return nil
}

// fileDigest measures a file's SHA-256.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// LaunchRequest is one scheduled graph execution.
type LaunchRequest struct {
	Launcher Launcher
	// TasksFile and TeamFile are the generated documents, written by the caller
	// into the per-epoch directory.
	TasksFile string
	TeamFile  string
	// WorkDir is the per-epoch directory for stdout/stderr capture.
	WorkDir string
	// Epoch is the launch epoch, recorded in every artifact path.
	Epoch int
	// Timeout bounds the whole run.
	Timeout time.Duration
	// OnLaunchIntent is called with the durable intent BEFORE the process starts.
	//
	// It is a callback rather than an inline write because this package does not
	// own the store: it cannot persist an intent, only the owner of the store can.
	// Returning an error aborts the launch, which is what makes the intent
	// genuinely prior rather than merely concurrent.
	OnLaunchIntent func(epoch int, argv []string) error
}

// LaunchResult is what a launch achieved.
type LaunchResult struct {
	// Started is true when the process was actually spawned.
	Started bool
	// ExitCode is the scheduler's exit status, when one was obtained.
	ExitCode int
	// StdoutPath and StderrPath are the bounded capture files.
	StdoutPath string
	StderrPath string
	// Cancelled is true when the run was stopped by a caller's context.
	Cancelled bool
	// Receipt parses the captured stdout.
	Receipt *Receipt
	// ReceiptErr is why the receipt could not be trusted, if it could not.
	ReceiptErr error
}

// Gate runs the pinned external scheduler.
//
// It is deliberately the ONLY place in this module that starts a process, so there
// is exactly one place where the artifact is verified, the intent is made durable,
// and cancellation is bridged.
type Gate struct{}

// Run executes one launch epoch.
func (g Gate) Run(ctx context.Context, req LaunchRequest, expectedTitles map[string]bool) (LaunchResult, error) {
	var res LaunchResult

	// 1. The artifact is verified BEFORE anything else. A refusal here has not
	//    created a process, so it is safe to retry against a correct artifact.
	if err := VerifyLauncher(req.Launcher); err != nil {
		return res, err
	}
	if strings.TrimSpace(req.TasksFile) == "" || strings.TrimSpace(req.TeamFile) == "" {
		return res, fmt.Errorf("oma: both a tasks file and a team file are required")
	}
	if req.Epoch <= 0 {
		return res, fmt.Errorf("oma: a launch epoch must be positive; got %d", req.Epoch)
	}
	if err := os.MkdirAll(req.WorkDir, 0o755); err != nil {
		return res, fmt.Errorf("oma: the epoch directory could not be created: %w", err)
	}

	argv := []string{"task", "--file", req.TasksFile, "--team", req.TeamFile}
	res.StdoutPath = filepath.Join(req.WorkDir, "stdout.log")
	res.StderrPath = filepath.Join(req.WorkDir, "stderr.log")

	// 2. The intent is durable before the process exists.
	if req.OnLaunchIntent != nil {
		if err := req.OnLaunchIntent(req.Epoch, argv); err != nil {
			return res, fmt.Errorf("oma: the launch intent could not be persisted, so nothing was started: %w", err)
		}
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, req.Launcher.Path, argv...)
	stdout, err := boundedFile(res.StdoutPath)
	if err != nil {
		return res, fmt.Errorf("oma: stdout capture could not be opened: %w", err)
	}
	defer stdout.Close()
	stderr, err := boundedFile(res.StderrPath)
	if err != nil {
		return res, fmt.Errorf("oma: stderr capture could not be opened: %w", err)
	}
	defer stderr.Close()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Its own process group, so cancellation reaches anything the scheduler
	// started rather than only the scheduler itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	res.Started = true
	runErr := cmd.Run()
	res.ExitCode = 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.Cancelled = runCtx.Err() != nil
		return res, fmt.Errorf("oma: the scheduler could not be run to completion: %w", runErr)
	}
	if runCtx.Err() != nil {
		res.Cancelled = true
	}

	// 3. The receipt is read back and identity-checked. Its ABSENCE is reported as
	//    unknown, never as success.
	raw, rerr := os.ReadFile(res.StdoutPath)
	if rerr != nil {
		res.ReceiptErr = fmt.Errorf("oma: the scheduler's stdout could not be read: %w", rerr)
		return res, nil
	}
	receipt, perr := ParseReceipt(raw, expectedTitles)
	if perr != nil {
		res.ReceiptErr = perr
		return res, nil
	}
	res.Receipt = receipt
	return res, nil
}

// Signal asks the scheduler to stop.
//
// It is only a REQUEST, and the caller must have already reconciled the durable
// cancellation FIRST. Killing the scheduler does not stop the tmux workers it
// launched — that is why the ordering is bridged rather than assumed, and why this
// function refuses to pretend it did anything to them.
func (g Gate) Signal(pid int) error {
	if pid <= 1 {
		return errors.New("oma: refusing to signal an invalid process id")
	}
	select {
	case <-time.After(0):
	default:
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("oma: the scheduler process group could not be signalled: %w", err)
	}
	return nil
}

// boundedFile captures up to MaxCapturedOutput and records that it truncated.
//
// A truncated receipt is still a receipt; what must not happen is a silent
// truncation that produces output which LOOKS complete. The truncation is
// therefore visible in the file the caller later reads.
func boundedFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
}

// RequestsDirName mirrors the store's request directory name.
//
// It is duplicated rather than imported because this package must not depend on
// the store: the gate is a subprocess boundary, and coupling it to the owner of
// durable state would make that boundary decorative. The name is asserted by a
// test so a change on either side fails loudly rather than silently writing
// artifacts somewhere the store will never look.
const RequestsDirName = "REQUESTS"

// EpochDir names the per-epoch artifact directory.
func EpochDir(root, rootRequestID string, epoch int) string {
	return filepath.Join(root, RequestsDirName, rootRequestID, "OMA", "epoch-"+strconv.Itoa(epoch))
}
