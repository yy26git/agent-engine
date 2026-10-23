// Command acp-probe establishes the real capability contract of the installed
// `mcode acp` server.
//
// It exists because the ACP plan's first gate (AP0) requires that protocol and
// artifact assumptions be REMOVED before the control layer is modified, and
// because every such assumption is cheap to make and expensive to discover later.
// This probe reads what the installed server advertises; it does not infer
// anything from documentation.
//
// It is deliberately disposable and deliberately narrow:
//
//   - It spawns only its own child process, in a caller-supplied directory.
//   - It NEVER sends a model prompt unless explicitly asked, because a prompt
//     costs tokens and can act on the filesystem.
//   - It records raw, bounded traces so the evidence can be re-read.
//
// Exit codes are meaningful so a later gate can branch on them:
//
//	0  handshake + session creation observed; capability matrix recorded
//	2  the server did not complete the handshake
//	3  session creation failed or was unsupported
//	4  a required selection surface was absent
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os/signal"
	"syscall"

	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/emjin/agent-engine/internal/minimaxacp"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MaxFrameBytes bounds one NDJSON frame. A server that emits an unbounded line
// must not be able to exhaust the probe's memory.
const MaxFrameBytes = 8 << 20 // 8 MiB

// Retain complete notification bodies within a separate trace budget. A partial
// body cannot establish that terminal model metadata was absent.
const maxTraceNotifications = 512
const maxTraceNotificationBytes = 4 << 20

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Params  json.RawMessage `json:"params"`
	Error   *rpcError       `json:"error"`
	Method  string          `json:"method"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// Error makes a JSON-RPC error usable as a Go error, which is what lets the
// method probe distinguish "method not found" from "session not found".
func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc %d: %s", e.Code, e.Message)
}

func main() { os.Exit(runProbe(os.Args[1:])) }

func runProbe(args []string) (exitCode int) {
	fs := flag.NewFlagSet("acp-probe", flag.ContinueOnError)
	launcher := fs.String("launcher", "", "absolute path to the pinned mcode launcher")
	cwd := fs.String("cwd", "", "disposable workspace for the probe session")
	out := fs.String("out", "", "directory to write the evidence trace")
	probeModel := fs.String("model", "", "exact advertised ACP model required for a prompt")
	modelLabel := fs.String("model-label", "", "selected model label to echo in the retained inspection conversation")
	probeEffort := fs.String("effort", "", "advertised thinking effort to select and read back after model selection")
	listSessions := fs.Bool("list-sessions", false, "list retained native conversations in cwd without creating a session or sending a prompt")
	probePrompt := fs.Bool("probe-prompt", false, "send one bounded harmless prompt (COSTS TOKENS, may act on files)")
	overall := fs.Duration("timeout", 4*time.Minute, "overall run deadline")
	handshakeTimeout := fs.Duration("handshake-timeout", 20*time.Second, "bounded initialize response deadline")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listSessions && *probePrompt {
		fmt.Fprintln(os.Stderr, "--list-sessions cannot be combined with --probe-prompt")
		return 2
	}

	if *launcher == "" || *cwd == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "acp-probe: --launcher, --cwd and --out are required")
		return 2
	}
	abs, err := filepath.Abs(*launcher)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acp-probe: %v\n", err)
		return 2
	}
	if !filepath.IsAbs(*launcher) {
		fmt.Fprintln(os.Stderr, "acp-probe: the launcher must be an absolute path")
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadlineCancel := context.WithTimeout(ctx, *overall)
	defer deadlineCancel()
	c := &client{ctx: ctx, done: make(chan struct{}), outDir: *out,
		trace: &evidence{Launcher: abs, Cwd: *cwd, Methods: map[string]string{}, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "RUNNING"}}
	defer func() {
		if caught := recover(); caught != nil {
			if f, ok := caught.(probeFailure); ok {
				exitCode = f.code
				c.trace.Failure = f.message
			} else {
				exitCode = 70
				c.trace.Failure = fmt.Sprint(caught)
			}
		}
		if c.trace.SessionID != "" && !c.trace.SessionClosed && c.w != nil {
			originalBoundary := c.trace.LastBoundary
			c.ctx = context.Background()
			if err := c.call("session/close", map[string]any{"sessionId": c.trace.SessionID}, 5*time.Second, nil); err != nil {
				c.trace.SessionCloseError = err.Error()
				if exitCode == 0 {
					exitCode = 6
				}
			}
			c.trace.LastBoundary = originalBoundary
		}
		close(c.done)
		if c.cmd != nil && c.cmd.Process != nil {
			outcome, err := minimaxacp.TerminateProbeProcess(c.cmd, time.Second)
			c.trace.Cleanup = outcome
			if err != nil {
				c.trace.CleanupError = err.Error()
				if exitCode == 0 {
					exitCode = 6
				}
			}
		}
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		if c.stderrFile != nil {
			_ = c.stderrFile.Close()
		}
		c.trace.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		c.trace.ExitCode = exitCode
		c.trace.Status = "FINISHED"
		if exitCode != 0 {
			c.trace.Status = "FAILED"
		}
		if err := c.trace.writeTrace(*out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			exitCode = 4
		}
	}()
	if err := c.checkpoint("before_spawn"); err != nil {
		fail(4, "initial evidence: %v", err)
	}
	manifest, identityErr := minimaxacp.MeasureRelease(abs)
	c.trace.ReleaseArtifacts = manifest
	if identityErr != nil {
		c.trace.ReleaseIdentityError = identityErr.Error()
	}
	if err := c.checkpoint("release_measured"); err != nil {
		fail(4, "release evidence: %v", err)
	}

	// stderr is kept separate from stdout on purpose: mixing them would corrupt
	// the NDJSON framing the protocol depends on.
	c.stderr, err = os.CreateTemp("", "acp-probe-stderr-*.log")
	if err != nil {
		fail(2, "stderr capture: %v", err)
	}
	c.trace.StderrPath = c.stderr.Name()
	_ = c.stderr.Close()

	c.cmd = exec.Command(abs, "acp")
	c.cmd.Dir = *cwd
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		fail(2, "stdin pipe: %v", err)
	}
	c.stdin = stdin

	// Both pipe handles are RETAINED on the client. Discarding them leaves the
	// *os.File reachable only through the Cmd, and a finalised file closes its
	// descriptor — which surfaces as the server's first response failing at
	// "read |1: file already closed".
	stdoutPipe, err := c.cmd.StdoutPipe()
	if err != nil {
		fail(2, "stdout pipe: %v", err)
	}
	c.stdout = stdoutPipe
	c.stderrFile, err = os.Create(c.stderr.Name())
	if err != nil {
		fail(2, "stderr file: %v", err)
	}
	c.cmd.Stderr = &boundedWriter{w: c.stderrFile, remaining: 1 << 20}
	if err := c.cmd.Start(); err != nil {
		fail(2, "starting the ACP server: %v", err)
	}
	c.trace.PID = c.cmd.Process.Pid
	if err := c.checkpoint("spawned"); err != nil {
		fail(4, "spawn evidence: %v", err)
	}

	c.stdin = stdin
	c.w = bufio.NewWriterSize(stdin, 1<<16)
	c.msgs = make(chan response, 256)

	go c.stream()

	// 1. initialize, advertising NO client capabilities. The plan forbids
	//    advertising filesystem or terminal handling before it is implemented,
	//    so the probe asks for nothing and records what the server offers anyway.
	init := map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}
	var initRes json.RawMessage
	if err := c.call("initialize", init, *handshakeTimeout, &initRes); err != nil {
		fail(2, "initialize failed: %v\n--- stderr ---\n%s", err, tailFile(c.stderr.Name()))
	}
	var initParsed struct {
		ProtocolVersion   int            `json:"protocolVersion"`
		AgentCapabilities map[string]any `json:"agentCapabilities"`
		AgentInfo         map[string]any `json:"agentInfo"`
		Meta              map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(initRes, &initParsed); err != nil {
		fail(2, "decoding initialize: %v", err)
	}
	c.trace.ProtocolVersion = initParsed.ProtocolVersion
	c.trace.AgentCapabilities = initParsed.AgentCapabilities
	c.trace.AgentInfo = initParsed.AgentInfo
	c.trace.Extensions = initParsed.Meta
	fmt.Printf("initialize: protocolVersion=%d agent=%v\n",
		initParsed.ProtocolVersion, initParsed.AgentInfo["name"])
	if *listSessions {
		if err := c.listWorkspaceSessions(*cwd); err != nil {
			fail(3, "session/list failed: %v", err)
		}
		return 0
	}

	// 2. session creation. No model call, no prompt.
	var sessRes json.RawMessage
	if err := c.call("session/new", map[string]any{"cwd": *cwd, "mcpServers": []any{}}, 30*time.Second, &sessRes); err != nil {
		fail(3, "session/new failed: %v\n--- stderr ---\n%s", err, tailFile(c.stderr.Name()))
	}
	var sess struct {
		SessionID     string           `json:"sessionId"`
		Modes         map[string]any   `json:"modes"`
		ConfigOptions []map[string]any `json:"configOptions"`
	}
	if err := json.Unmarshal(sessRes, &sess); err != nil {
		fail(3, "decoding session/new: %v", err)
	}
	c.trace.SessionID = sess.SessionID
	c.trace.ConfigOptions = sess.ConfigOptions
	fmt.Printf("session/new: sessionId=%s configOptions=%d\n", sess.SessionID, len(sess.ConfigOptions))

	// 3. Probe the selection surface WITH THE REAL SESSION ID. Method-not-found
	//    and resource-not-found are different answers and only the second one
	//    proves the method exists — which is why this cannot be a blind pipe.
	fmt.Println("\nselection surface (real session id):")
	for _, probe := range []struct {
		method string
		params map[string]any
	}{
		{"session/set_mode", map[string]any{"sessionId": sess.SessionID, "modeId": "plan"}},
		{"session/set_config", map[string]any{"sessionId": sess.SessionID, "configId": "permissionMode", "value": "default"}},
		{"session/set_config_option", map[string]any{"sessionId": sess.SessionID, "configId": "permissionMode", "value": "default"}},
		{"session/set_permission_mode", map[string]any{"sessionId": sess.SessionID, "value": "default"}},
		{"session/set_model", map[string]any{"sessionId": sess.SessionID, "value": "m:minimax:MiniMax-M3:v:thinking"}},
		{"session/config/set", map[string]any{"sessionId": sess.SessionID, "configId": "permissionMode", "value": "default"}},
		{"session/set_option", map[string]any{"sessionId": sess.SessionID, "optionId": "permissionMode", "value": "default"}},
		{"mcode/session/config/set", map[string]any{"sessionId": sess.SessionID, "configId": "permissionMode", "value": "default"}},
		{"session/cancel", map[string]any{"sessionId": sess.SessionID}},
		{"mcode/session/cancel", map[string]any{"sessionId": sess.SessionID}},
		{"session/activate", map[string]any{"sessionId": sess.SessionID}},
		{"mcode/session/queue/list", map[string]any{"sessionId": sess.SessionID}},
	} {
		var res json.RawMessage
		err := c.call(probe.method, probe.params, 12*time.Second, &res)
		switch {
		case err == nil:
			c.trace.Methods[probe.method] = "supported: " + string(res)
			fmt.Printf("  %-32s SUPPORTED %s\n", probe.method, truncate(string(res), 80))
		case rpcErr(err) != nil:
			code := rpcErr(err).Code
			verdict := "absent (-32601 method not found)"
			if code != -32601 {
				verdict = fmt.Sprintf("present (-%d %s)", code, rpcErr(err).Message)
			}
			c.trace.Methods[probe.method] = verdict
			fmt.Printf("  %-32s %s\n", probe.method, verdict)
		default:
			c.trace.Methods[probe.method] = "unknown: " + err.Error()
			fmt.Printf("  %-32s UNKNOWN\n", probe.method)
		}
	}

	// 3b. Does set_config_option actually CHANGE the value, or merely echo it?
	//     An echo would be worse than absence: it would let a caller believe it had
	//     confined a worker that was never reconfigured.
	fmt.Println("\nconfig change verification (read back, not echo):")
	current := map[string]string{}
	for _, c := range sess.ConfigOptions {
		current[c["id"].(string)], _ = c["currentValue"].(string)
	}
	fmt.Printf("  initial      %v\n", current)
	for _, set := range []struct{ id, value string }{
		{"permissionMode", "default"},
		{"permissionMode", "auto"},
		{"model", "m:minimax:MiniMax-M2.7:v:thinking"},
		{"permissionMode", "notARealMode"},
	} {
		var res json.RawMessage
		err := c.call("session/set_config_option", map[string]any{
			"sessionId": sess.SessionID, "configId": set.id, "value": set.value,
		}, 12*time.Second, &res)
		if err != nil {
			fmt.Printf("  set %-15s = %-34s REFUSED %v\n", set.id, set.value, rpcErr(err))
			c.trace.ConfigChanges = append(c.trace.ConfigChanges,
				fmt.Sprintf("%s=%s refused: %v", set.id, set.value, err))
			continue
		}
		var out struct {
			ConfigOptions []struct {
				ID           string `json:"id"`
				CurrentValue string `json:"currentValue"`
			} `json:"configOptions"`
		}
		_ = json.Unmarshal(res, &out)
		now := map[string]string{}
		for _, o := range out.ConfigOptions {
			now[o.ID] = o.CurrentValue
		}
		verdict := "UNCHANGED"
		if before, had := current[set.id]; had && now[set.id] != before {
			verdict = "CHANGED"
			current[set.id] = now[set.id]
		}
		fmt.Printf("  set %-15s = %-34s %-9s -> %s\n", set.id, set.value, verdict, now[set.id])
		c.trace.ConfigChanges = append(c.trace.ConfigChanges,
			fmt.Sprintf("%s=%s %s (now %q)", set.id, set.value, verdict, now[set.id]))
	}

	// 4. Optional, explicit, and never the default.
	if *probePrompt {
		if c.trace.ReleaseIdentityError != "" {
			fail(3, "prompt refused without complete release identity: %s", c.trace.ReleaseIdentityError)
		}
		if err := c.selectPromptConfiguration(sess.SessionID, *probeModel, sess.ConfigOptions, *probeEffort); err != nil {
			fail(3, "prompt selection refused: %v", err)
		}
		fmt.Println("\nprobe prompt: sending ONE bounded harmless prompt")
		promptText := "Reply with exactly: ACP_PROBE_OK"
		if *modelLabel != "" {
			promptText = "Agent Engine desktop inspection probe. Do not use tools or modify files. Reply with " + *modelLabel + "."
			c.trace.RequestedModelEcho = *modelLabel
		}
		prompt := map[string]any{
			"sessionId": sess.SessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": promptText}},
		}
		var res json.RawMessage
		if err := c.call("session/prompt", prompt, 120*time.Second, &res); err != nil {
			fmt.Printf("  prompt failed: %v\n", err)
			c.trace.Prompt = "failed: " + err.Error()
		} else {
			c.trace.Prompt = "ok: " + string(res)
			var turn struct {
				Model      string `json:"model"`
				StopReason string `json:"stopReason"`
			}
			if err := json.Unmarshal(res, &turn); err == nil {
				c.trace.AnswerModel = turn.Model
				c.trace.StopReason = turn.StopReason
			}
			fmt.Printf("  prompt -> %s\n", truncate(string(res), 300))
		}
	} else {
		fmt.Println("\nprompt: SKIPPED (pass -probe-prompt to send one; it costs tokens and may write files)")
	}

	// Cancellation is a NOTIFICATION, not a request: it carries no id and returns
	// nothing. Sending it as a request yields -32601 and looks like an absent
	// method. A written notification proves transmission only, not acceptance.
	fmt.Println("\ncancel as notification (no id, expects no response):")
	if err := c.notify("session/cancel", map[string]any{"sessionId": sess.SessionID}); err != nil {
		fmt.Printf("  write failed: %v\n", err)
	} else {

		fmt.Println("  session/cancel  -> notification written; acceptance and cancellation unverified")
		c.trace.Methods["session/cancel"] = "notification written; acceptance and active cancellation UNKNOWN"
	}

	// close LAST. A probe that closes before verifying config and before the
	// optional prompt observes every later call as "resource not found" and
	// would misreport two working capabilities as absent.
	fmt.Println("\nshutdown:")
	var closeRes json.RawMessage
	if err := c.call("session/close", map[string]any{"sessionId": sess.SessionID}, 12*time.Second, &closeRes); err != nil {
		fmt.Printf("  session/close -> %v\n", err)
		c.trace.Methods["session/close"] = "error: " + err.Error()
	} else {
		fmt.Println("  session/close -> SUPPORTED")
		c.trace.Methods["session/close"] = "supported: " + string(closeRes)
	}

	if err := c.trace.writeTrace(*out); err != nil {
		fail(4, "writing evidence: %v", err)
	}
	fmt.Printf("\nevidence written to %s\n", *out)
	if strings.HasPrefix(c.trace.Prompt, "failed:") {
		return 5
	}
	return 0
}

type evidence struct {
	StartedAt            string            `json:"started_at"`
	EndedAt              string            `json:"ended_at,omitempty"`
	Status               string            `json:"status"`
	ExitCode             int               `json:"exit_code"`
	LastBoundary         string            `json:"last_boundary"`
	PID                  int               `json:"pid,omitempty"`
	Failure              string            `json:"failure,omitempty"`
	Cleanup              string            `json:"cleanup,omitempty"`
	CleanupError         string            `json:"cleanup_error,omitempty"`
	StderrPath           string            `json:"stderr_path,omitempty"`
	LauncherSHA256       string            `json:"launcher_sha256,omitempty"`
	ReleaseArtifacts     map[string]string `json:"release_artifacts,omitempty"`
	ReleaseIdentityError string            `json:"release_identity_error,omitempty"`

	Launcher             string            `json:"launcher"`
	Cwd                  string            `json:"cwd"`
	ProtocolVersion      int               `json:"protocol_version"`
	AgentCapabilities    map[string]any    `json:"agent_capabilities"`
	AgentInfo            map[string]any    `json:"agent_info"`
	Extensions           map[string]any    `json:"extensions"`
	SessionID            string            `json:"session_id"`
	SessionClosed        bool              `json:"session_closed"`
	SessionCloseError    string            `json:"session_close_error,omitempty"`
	ConfigOptions        []map[string]any  `json:"config_options"`
	Methods              map[string]string `json:"method_probe"`
	Notifications        []map[string]any  `json:"notifications,omitempty"`
	NotificationCount    int               `json:"notification_count"`
	NotificationDropped  int               `json:"notification_dropped"`
	NotificationBytes    int               `json:"notification_bytes"`
	ConfigChanges        []string          `json:"config_changes,omitempty"`
	Prompt               string            `json:"prompt,omitempty"`
	RequestedModel       string            `json:"requested_model,omitempty"`
	RequestedModelEcho   string            `json:"requested_model_echo,omitempty"`
	RequestedEffort      string            `json:"requested_effort,omitempty"`
	ConfiguredEffort     string            `json:"configured_effort,omitempty"`
	Sessions             []sessionInfo     `json:"sessions,omitempty"`
	ConfiguredModel      string            `json:"configured_model,omitempty"`
	ConfiguredPermission string            `json:"configured_permission,omitempty"`
	AnswerModel          string            `json:"answer_model,omitempty"`
	StopReason           string            `json:"stop_reason,omitempty"`
}

type sessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title,omitempty"`
}

func (c *client) listWorkspaceSessions(cwd string) error {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 16; page++ {
		params := map[string]any{"cwd": cwd}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Sessions   []sessionInfo `json:"sessions"`
			NextCursor string        `json:"nextCursor"`
		}
		var raw json.RawMessage
		if err := c.call("session/list", params, 30*time.Second, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		for _, session := range result.Sessions {
			if session.SessionID == "" || filepath.Clean(session.Cwd) != filepath.Clean(cwd) {
				return fmt.Errorf("session/list returned a missing identity or a different workspace")
			}
			if len(c.trace.Sessions) >= 256 {
				return fmt.Errorf("session/list exceeded its 256-conversation evidence bound")
			}
			c.trace.Sessions = append(c.trace.Sessions, session)
			title := session.Title
			if title == "" {
				title = "<unnamed>"
			}
			fmt.Printf("%s  %q\n", session.SessionID, title)
		}
		if result.NextCursor == "" {
			return c.checkpoint("sessions_listed_without_activation")
		}
		if seen[result.NextCursor] {
			return fmt.Errorf("session/list repeated its pagination cursor")
		}
		seen[result.NextCursor] = true
		cursor = result.NextCursor
	}
	return fmt.Errorf("session/list exceeded its pagination bound")
}

func (e *evidence) writeTrace(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".evidence-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, filepath.Join(dir, "acp-capability-evidence.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type client struct {
	ctx        context.Context
	done       chan struct{}
	outDir     string
	cmd        *exec.Cmd
	w          *bufio.Writer
	msgs       chan response
	stderr     *os.File
	stderrFile *os.File
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	nextID     int
	streamErr  error
	trace      *evidence
}

// stream is the single reader of stdout. One goroutine owns the pipe for the
// life of the process.
//
// A per-call reader is wrong in a way that only shows up under timeout: the
// abandoned goroutine keeps consuming frames, so the next call can miss its own
// response or a timeout can silently steal a notification.
func (c *client) stream() {
	defer close(c.msgs)
	rd := io.Reader(c.stdout)
	if c.stdout == nil {
		c.streamErr = errors.New("stdout pipe was not retained")
		return
	}
	// The protocol is NDJSON — one JSON object per LINE — so a line scanner is
	// both the simpler and the correct reader. A json.Decoder would read ahead
	// across line boundaries and turn a framing error into an unrelated
	// "file already closed", which is exactly what it did here.
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, 1<<20), MaxFrameBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m response
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			c.streamErr = fmt.Errorf("protocol corruption: %w", err)
			return
		}
		select {
		case c.msgs <- m:
		case <-c.done:
			return
		}
	}
	c.streamErr = sc.Err()
}

// call sends one request and waits for its correlated response.
//
// Correlation by id is what makes a concurrent method probe meaningful: a
// notification or a late response cannot be mistaken for this request's answer.
func (c *client) call(method string, params any, timeout time.Duration, out *json.RawMessage) error {
	if c.ctx != nil && c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if err := c.checkpoint("send:" + method); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var cancelled <-chan struct{}
	if c.ctx != nil {
		cancelled = c.ctx.Done()
	}
	c.nextID++
	id := c.nextID
	raw, err := json.Marshal(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if pipe, ok := c.stdin.(*os.File); ok {
		until := time.Now().Add(timeout)
		if c.ctx != nil {
			if deadline, ok := c.ctx.Deadline(); ok && deadline.Before(until) {
				until = deadline
			}
		}
		if err := pipe.SetWriteDeadline(until); err != nil {
			return err
		}
	}
	if _, err := c.w.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}

	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				if c.streamErr != nil {
					return fmt.Errorf("stream ended: %w", c.streamErr)
				}
				return errors.New("the server closed stdout without answering")
			}
			// DEFECT 2 FIX: notifications were discarded. They are retained,
			// because "did any update acknowledge THIS prompt" is a question AP0
			// must answer and a dropped notification answers it by silence.
			if m.ID == nil || m.Method != "" {
				if m.Method != "" {
					c.record(m)
				}
				continue
			}
			if err := c.checkpoint("received:" + method); err != nil {
				return err
			}
			if *m.ID != id {
				continue // a late response to an earlier, already-timed-out call
			}
			if m.Error != nil {
				return m.Error
			}
			if method == "session/close" {
				c.trace.SessionClosed = true
				c.trace.SessionCloseError = ""
			}
			if out != nil {
				*out = m.Result
			}
			return nil
		case <-cancelled:
			return c.ctx.Err()
		case <-timer.C:
			return fmt.Errorf("no response within %s", timeout)
		}
	}
}

// record retains complete notifications or explicitly records their omission.
// A nonzero dropped count makes absence claims from this trace inconclusive.
func (c *client) record(m response) {
	c.trace.NotificationCount++
	size := len(m.Method) + len(m.Params)
	if len(c.trace.Notifications) >= maxTraceNotifications || size > maxTraceNotificationBytes-c.trace.NotificationBytes {
		c.trace.NotificationDropped++
		return
	}
	c.trace.Notifications = append(c.trace.Notifications, map[string]any{
		"sequence": c.trace.NotificationCount,
		"method":   m.Method,
		"body":     string(m.Params),
	})
	c.trace.NotificationBytes += size
}

// notify sends a JSON-RPC NOTIFICATION: no id, and therefore no response.
//
// `session/cancel` is one of these, which is exactly why sending it as a request
// produced -32601 and looked like an absent method.
func (c *client) notify(method string, params any) error {
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	if _, err := c.w.Write(append(raw, '\n')); err != nil {
		return err
	}
	return c.w.Flush()
}

func rpcErr(err error) *rpcError {
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	return nil
}

type probeFailure struct {
	code    int
	message string
}

func fail(code int, format string, args ...any) {
	panic(probeFailure{code, fmt.Sprintf(format, args...)})
}

func (c *client) checkpoint(boundary string) error {
	c.trace.LastBoundary = boundary
	if c.trace.LauncherSHA256 == "" && c.trace.Launcher != "" {
		if data, err := os.ReadFile(c.trace.Launcher); err == nil {
			sum := sha256.Sum256(data)
			c.trace.LauncherSHA256 = hex.EncodeToString(sum[:])
		}
	}
	return c.trace.writeTrace(c.outDir)
}

type boundedWriter struct {
	w         io.Writer
	remaining int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.remaining <= 0 {
		return n, nil
	}
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	written, err := w.w.Write(p)
	w.remaining -= written
	if err != nil {
		return written, err
	}
	return n, nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func tailFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "(no stderr)"
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, 4096))
	return string(data)
}

// Configuration read-back is retained separately from answer-turn attribution.
// A configured model is never substituted for a missing model in the prompt result.
func (c *client) selectPromptConfiguration(sessionID, model string, advertised []map[string]any, effort ...string) error {
	c.trace.RequestedModel = model
	if model == "" {
		return fmt.Errorf("--model is required; a prompt cannot silently use the default")
	}
	type option struct {
		ID      string `json:"id"`
		Current string `json:"currentValue"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	raw, err := json.Marshal(advertised)
	if err != nil {
		return err
	}
	var options []option
	if err := json.Unmarshal(raw, &options); err != nil {
		return err
	}
	selections := []struct{ id, value string }{{"model", model}, {"permissionMode", "default"}}
	if len(effort) > 0 && effort[0] != "" {
		c.trace.RequestedEffort = effort[0]
		selections = append(selections, struct{ id, value string }{"thinkingEffort", effort[0]})
	}
	for _, selection := range selections {
		known := false
		for _, opt := range options {
			if opt.ID == selection.id {
				for _, v := range opt.Options {
					if v.Value == selection.value {
						known = true
					}
				}
			}
		}
		if !known {
			return fmt.Errorf("%s=%q was not advertised", selection.id, selection.value)
		}
		var result json.RawMessage
		if err := c.call("session/set_config_option", map[string]any{"sessionId": sessionID, "configId": selection.id, "value": selection.value}, 12*time.Second, &result); err != nil {
			return err
		}
		var readback struct {
			Options []option `json:"configOptions"`
		}
		if err := json.Unmarshal(result, &readback); err != nil {
			return err
		}
		// Model selection may advertise additional controls, such as effort.
		options = readback.Options
		value := ""
		for _, opt := range readback.Options {
			if opt.ID == selection.id {
				value = opt.Current
			}
		}
		if selection.id == "model" {
			c.trace.ConfiguredModel = value
		} else if selection.id == "thinkingEffort" {
			c.trace.ConfiguredEffort = value
		} else {
			c.trace.ConfiguredPermission = value
		}
		if err := c.checkpoint("prompt_selection:" + selection.id); err != nil {
			return err
		}
		if value != selection.value {
			return fmt.Errorf("%s selection mismatch: requested %q, read back %q", selection.id, selection.value, value)
		}
	}
	return nil
}
