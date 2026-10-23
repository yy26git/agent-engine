package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/minimaxacp"
	"github.com/emjin/agent-engine/internal/runtime"
)

type ownershipEvidence struct {
	PID                   int    `json:"pid"`
	StartToken            string `json:"start_token"`
	Tool                  string `json:"tool"`
	ToolResult            string `json:"tool_result"`
	TeardownReportedClean bool   `json:"teardown_reported_clean"`
	TeardownError         string `json:"teardown_error,omitempty"`
	AliveAfterTeardown    bool   `json:"alive_after_teardown"`
	WroteAfterTeardown    bool   `json:"wrote_after_teardown"`
	FixtureCleanupProven  bool   `json:"fixture_cleanup_proven"`
	TeardownCompletedAt   string `json:"teardown_completed_at,omitempty"`
	SourceAtTeardown      string `json:"source_at_teardown_sha256,omitempty"`
	SourceAfterTeardown   string `json:"source_after_teardown_sha256,omitempty"`
	LateWriteObservedAt   string `json:"late_write_observed_at,omitempty"`
	Passed                bool   `json:"passed"`
}

// The helper is fixed, self-terminating, and writes only the disposable source.
// Its identity is recorded from the real tool result before teardown; no host
// session search or broad process-name kill is used for fixture cleanup.
func probeDetachedOwnership(ctx context.Context, conn *minimaxacp.Connection, ref runtime.SessionRef, f *fixtureProvider, node, source string) (e *ownershipEvidence, resultErr error) {
	e = &ownershipEvidence{}
	before, err := os.ReadFile(source)
	if err != nil {
		return e, err
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	childJS := `setTimeout(()=>{require('node:fs').appendFileSync(process.argv[1],'DETACHED_WRITE\n');process.exit(0)},8000);`
	parentJS := `const cp=require('node:child_process');const child=cp.spawn(process.execPath,['-e',` + strconv.Quote(childJS) + `,process.argv[1]],{detached:true,stdio:'ignore'});child.on('error',err=>{console.error(err);process.exit(1)});child.on('spawn',()=>{console.log('DETACHED_PID='+child.pid);child.unref()});`
	p := &probe{Name: "detached-writer-ownership", ToolKind: "shell", Target: quote(node) + " -e " + quote(parentJS) + " " + quote(source)}
	f.mu.Lock()
	f.current = p
	f.mu.Unlock()
	_, err = conn.Submit(ctx, ref, "Run the one fixed disposable ownership probe. Do not call other tools.")
	f.mu.Lock()
	e.Tool, e.ToolResult = p.Called, p.Result
	f.mu.Unlock()
	if err != nil {
		return e, err
	}
	match := regexp.MustCompile(`DETACHED_PID=([1-9][0-9]*)`).FindStringSubmatch(e.ToolResult)
	if e.Tool == "" || len(match) != 2 {
		return e, errors.New("detached probe has no correlated real child identity")
	}
	e.PID, err = strconv.Atoi(match[1])
	if err != nil {
		return e, err
	}
	e.StartToken = control.StartTokenFor(e.PID)
	if e.StartToken == "" {
		return e, errors.New("detached probe child was not live before teardown; ownership unproven")
	}
	defer func() {
		// Kill only our measured fixed helper if it has not already self-exited.
		if control.StartTokenFor(e.PID) == e.StartToken {
			_ = syscall.Kill(e.PID, syscall.SIGTERM)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if errors.Is(syscall.Kill(e.PID, 0), syscall.ESRCH) {
				e.FixtureCleanupProven = true
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		resultErr = errors.Join(resultErr, errors.New("detached qualification helper cleanup unproven"))
	}()
	preTeardown, err := os.ReadFile(source)
	if err != nil || string(preTeardown) != string(before) {
		return e, errors.New("detached writer ran before teardown; timing precondition unproven")
	}
	teardown := conn.Teardown(context.Background())
	e.TeardownCompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	e.TeardownReportedClean = teardown.Clean()
	if err := teardown.Err(); err != nil {
		e.TeardownError = err.Error()
	}
	postTeardown, err := os.ReadFile(source)
	if err != nil {
		return e, err
	}
	atTeardown := sha256.Sum256(postTeardown)
	e.SourceAtTeardown = hex.EncodeToString(atTeardown[:])
	if string(postTeardown) != string(before) {
		return e, errors.New("source changed during teardown; later-write timing unproven")
	}
	e.AliveAfterTeardown = control.StartTokenFor(e.PID) == e.StartToken
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		content, err := os.ReadFile(source)
		if err != nil {
			return e, err
		}
		if string(content) != string(before) {
			after := sha256.Sum256(content)
			e.SourceAfterTeardown = hex.EncodeToString(after[:])
			e.LateWriteObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if string(content) != string(before)+"DETACHED_WRITE\n" {
				return e, errors.New("source change differs from the fixed detached helper effect")
			}
			e.WroteAfterTeardown = true
			return e, fmt.Errorf("detached writer %d changed source after teardown (reported_clean=%v); ownership qualification refused", e.PID, e.TeardownReportedClean)
		}
		if errors.Is(syscall.Kill(e.PID, 0), syscall.ESRCH) {
			e.Passed = !e.AliveAfterTeardown && e.TeardownReportedClean
			if !e.Passed {
				return e, errors.New("detached writer survived teardown; later exit is not a proven termination boundary")
			}
			return e, nil
		}
		select {
		case <-ctx.Done():
			return e, ctx.Err()
		case <-deadline.C:
			return e, errors.New("detached writer ownership deadline expired")
		case <-ticker.C:
		}
	}
}
