// Package minimaxacp is a native ACP client for the pinned `mcode acp` server.
//
// It implements runtime.Backend over a directly-owned child process. Nothing
// here decides anything: it starts a process, negotiates what that process can
// actually do, routes frames, and reports what it observed. Approval, obligations
// and results belong to Store and Engine, and a client that could approve work
// would make every boundary in the system decorative.
//
// Four decisions carry the weight, and each answers a specific way this goes
// wrong in the field.
//
// The executable is PINNED by digest and launched as an ABSOLUTE path. Resolving
// it through PATH is how a reviewed client ends up running something else.
//
// Capabilities are DISCOVERED, never assumed. The installed server advertises
// what it supports; anything not advertised is refused rather than attempted, and
// a configuration value the server rejects is a refusal, not a fallback to the
// default. A worker silently running a different model than the one admitted is a
// false completion.
//
// Client filesystem and terminal capabilities are NOT advertised. The server
// grants the client those surfaces, and an unimplemented handler is a handler
// that will eventually be invoked by something and fail open. Until handlers
// exist with workspace validation, the client asks for nothing.
//
// Writes are SERIALIZED and exactly one prompt may be in flight per session.
// The protocol is JSON-RPC, which tolerates interleaving; a coding agent's turn
// does not, and the failure mode of two concurrent prompts is a tool applied to
// the wrong state rather than an error.
package minimaxacp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// BackendName identifies this transport in provenance and bindings.
const BackendName = "minimax-acp"

// MaxFrameBytes bounds one NDJSON frame.
//
// The protocol is line-framed, so a server that emits an unbounded line must not
// be able to exhaust this client's memory. An oversized frame is a refusal, not a
// truncation: a partial frame decoded as a whole one is how a transport invents
// an approval nobody gave.
const MaxFrameBytes = 8 << 20 // 8 MiB

// Bounds applied to every connection. They are positive and finite because an
// unbounded read or write in a supervision path is an outage waiting for a slow
// peer.
const (
	DefaultHandshakeTimeout = 30 * time.Second
	DefaultCallTimeout      = 60 * time.Second
	DefaultTurnTimeout      = 15 * time.Minute
	DefaultShutdownGrace    = 10 * time.Second
	MaxQueueDepth           = 256
	// MaxReceiptQueueDepth bounds the correlated-receipt stream.
	//
	// It is smaller than the notification bound on purpose: a receipt is rare and
	// load-bearing, while notifications are frequent and merely informative. A
	// backlog of notifications must never be able to consume the budget that
	// completions need.
	MaxReceiptQueueDepth = 64
)

// ErrNotAuthenticated marks the operations that require a signed-in account.
//
// It is distinct from "unsupported" because it is an OPERATIONAL condition, not a
// protocol fact, and the correct response is different: retry after a login, never
// fall back to a weaker transport.
var ErrNotAuthenticated = errors.New("minimax-acp: the server requires an authenticated account")

// Unsupported reports whether a named server capability is absent.
func Unsupported(what string) error {
	return fmt.Errorf("%w: the server does not advertise %s", runtime.ErrUnsupported, what)
}

// Options configures one ACP connection.
type Options struct {
	// BackendID identifies an isolated process for one admitted profile.
	BackendID string
	// Launcher is the ABSOLUTE path to the pinned `mcode` release launcher.
	Launcher string
	// LauncherSHA256 is the measured digest of that launcher.
	LauncherSHA256 string
	// Cwd is the workspace the session runs in.
	Cwd string
	// DataDir selects an isolated stock MiniMax profile for qualification. Empty
	// preserves the operator's normal profile. It never copies credentials.
	DataDir string
	// Boundary is an optional whole-process candidate for qualification. Production
	// admission must independently qualify its scope before enabling it.
	Boundary *ProcessBoundary
	// Model and PermissionMode are the FROZEN selections applied at session
	// creation. Empty means "whatever the server defaults to", which is recorded
	// rather than assumed.
	Model          string
	Effort         string
	PermissionMode string
	// StateDir is where durable transport state lives.
	//
	// It holds the receipt spool. A terminal receipt is the only durable record
	// that a turn completed, so losing it silently ends the ability to assess the
	// work. Empty means no spool, and publishReceipt then degrades the connection
	// rather than pretending the completion was recorded.
	StateDir string
	// OnSpawned is invoked IMMEDIATELY after the child process is created and
	// BEFORE the handshake.
	//
	// It exists because a handshake is not instantaneous, and a supervisor that
	// dies during it leaves a real process behind. Without a callback here the
	// earliest durable record of that process is a binding written much later, so
	// the crash window is unrecorded and nothing can ever reap it.
	//
	// It returns an error to ABORT startup. A spawn this supervisor cannot record
	// is a spawn it must not keep: continuing would leave a process nobody can
	// name, and the next run would admit a replacement beside it.
	OnSpawned func(pid int) error
	// HandshakeTimeout, CallTimeout and ShutdownGrace bound the connection.
	HandshakeTimeout time.Duration
	CallTimeout      time.Duration
	// TurnTimeout is separate from short configuration RPCs. A coding turn
	// may exceed a minute; its caller's episode deadline remains authoritative.
	TurnTimeout   time.Duration
	ShutdownGrace time.Duration
}

// Connection is one owned ACP child process.
type Connection struct {
	opts Options

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	w      *bufio.Writer
	msgs   chan frame
	stderr *os.File

	seq     int
	pending map[int]chan frame
	// closed is closed once the reader ends, so every waiter is released.
	closed   chan struct{}
	closeErr error

	// caps is whatever the handshake advertised. Nothing is attempted that is not
	// in here.
	caps       ServerCapabilities
	agent      AgentInfo
	negociated bool

	writeMu sync.Mutex
	updates chan runtime.Event
	// receipts is a SEPARATE stream from updates, and the separation is the fix.
	//
	// A single bounded channel shared by both meant a flood of tool-call
	// notifications could fill it and starve the one receipt that means "the turn
	// ended". The result was silent: the completion simply never arrived, and a
	// supervisor with nothing to assess looks exactly like one with nothing to do.
	receipts chan runtime.Event

	// openSessions are tracked so Teardown can close them gracefully before it
	// resorts to a process-group signal.
	openSessions map[string]bool
	// closedSessions are sessions this connection closed ON PURPOSE.
	//
	// It is the difference between a session that finished and one that vanished.
	// Both are "not open", and a supervisor that cannot tell them apart either
	// deletes a finished session or re-dispatches to a dead one — so the fact of
	// closing is recorded rather than inferred from absence.
	closedSessions map[string]bool
	// openLabels maps an activation label to the session carrying it.
	//
	// Without it, Reconcile could only answer "some session exists", which is not
	// an answer: adopting another activation's session is how a worker bound to
	// the wrong label ends up looking healthy.
	openLabels map[string]string
	// activePrompt is the single in-flight prompt, if any.
	activePrompt       string
	promptText         string
	promptTextOverflow bool
	// sendSeq numbers receipts so two prompts to one session have distinct ids.
	sendSeq atomic.Uint64
	// updateSeq numbers notifications at receipt, before bounded queue admission.
	// Receipts have a separate durable identity and never consume these ordinals.
	updateSeq      atomic.Uint64
	droppedUpdates int
	// spool is the durable receipt log; spoolMu serialises append and compaction.
	spool     *os.File
	spoolPath string
	spoolMu   sync.Mutex
	// ackedReceipts are receipts the consumer has durably recorded.
	ackedReceipts map[string]bool
	// droppedReceipts counts receipts the bounded receipt stream could not carry.
	// It is counted rather than hidden, because a dropped receipt is a completion
	// that will silently never be assessed.
	droppedReceipts int
	// degraded records that at least one receipt was lost.
	degraded bool
	// teardownOnce, teardownDone and teardownRes make teardown idempotent.
	//
	// Cleanup can be reached from a handshake failure, a cancellation, a
	// replacement and a supervisor shutdown, sometimes concurrently. Signalling
	// the same pid twice is not merely wasteful: once the child is gone that pid
	// can be REUSED, and a second signal would then land on a stranger. The
	// verdict is therefore computed once and every later caller reads it.
	teardownOnce sync.Once
	teardownDone chan struct{}
	teardownRes  TeardownResult
	// terminateWith overrides how Teardown signals. nil means the real syscall.
	terminateWith func(int, syscall.Signal) error
}

// Compile-time proof the client satisfies the boundary it was written for.
var _ runtime.Backend = (*Connection)(nil)

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc %d: %s", e.Code, e.Message) }

// ServerCapabilities is what the handshake advertised.
type ServerCapabilities struct {
	ProtocolVersion   int  `json:"protocolVersion"`
	LoadSession       bool `json:"loadSession"`
	SessionList       bool `json:"-"`
	SessionResume     bool `json:"-"`
	SessionClose      bool `json:"-"`
	MCPHTTP           bool `json:"-"`
	MCPStream         bool `json:"-"`
	PromptImage       bool `json:"promptCapabilities.image"`
	PromptAudio       bool `json:"promptCapabilities.audio"`
	PromptEmbeddedCtx bool `json:"promptCapabilities.embeddedContext"`
}

// AgentInfo identifies the server.
type AgentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion int             `json:"protocolVersion"`
	AgentCaps       json.RawMessage `json:"agentCapabilities"`
	AgentInfo       AgentInfo       `json:"agentInfo"`
	Meta            map[string]any  `json:"_meta"`
}

// Start launches the pinned server and completes the handshake.
//
// The launcher digest is verified BEFORE the process is spawned, so a substituted
// executable never runs at all — not even to discover that it was wrong.
func Start(ctx context.Context, opts Options) (*Connection, error) {
	if opts.DataDir != "" && !filepath.IsAbs(opts.DataDir) {
		return nil, errors.New("minimax-acp: data directory must be absolute")
	}

	if err := verifyLauncher(opts); err != nil {
		return nil, err
	}
	command, boundaryErr := boundaryCommand(opts)
	if boundaryErr != nil {
		return nil, boundaryErr
	}
	if strings.TrimSpace(opts.Cwd) == "" {
		return nil, errors.New("minimax-acp: a workspace is required")
	}

	c := &Connection{
		opts:           opts,
		pending:        map[int]chan frame{},
		closed:         make(chan struct{}),
		updates:        make(chan runtime.Event, MaxQueueDepth),
		receipts:       make(chan runtime.Event, MaxReceiptQueueDepth),
		ackedReceipts:  map[string]bool{},
		openSessions:   map[string]bool{},
		closedSessions: map[string]bool{},
		openLabels:     map[string]string{},
		teardownDone:   make(chan struct{}),
	}
	if c.opts.HandshakeTimeout <= 0 {
		c.opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.opts.CallTimeout <= 0 {
		c.opts.CallTimeout = DefaultCallTimeout
	}
	if c.opts.ShutdownGrace <= 0 {
		c.opts.ShutdownGrace = DefaultShutdownGrace
	}

	// The spool is opened BEFORE the process starts. A receipt log that cannot be
	// opened is a transport that cannot make a completion durable, and finding that
	// out mid-episode — after a turn has ended — is far too late.
	if dir := strings.TrimSpace(opts.StateDir); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("minimax-acp: the state directory %s is unusable: %w", dir, err)
		}
		path := filepath.Join(dir, "receipts.ndjson")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("minimax-acp: the receipt spool %s could not be opened: %w", path, err)
		}
		c.spool, c.spoolPath = f, path
	}

	c.cmd = command
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "AGENT_ENGINE_LOCAL_TOKEN=") && !(opts.DataDir != "" && strings.HasPrefix(entry, "MINIMAX_DATA_DIR=")) && !(opts.Boundary != nil && strings.HasPrefix(entry, "TMPDIR=")) {
			c.cmd.Env = append(c.cmd.Env, entry)
		}
	}
	if opts.DataDir != "" {
		c.cmd.Env = append(c.cmd.Env, "MINIMAX_DATA_DIR="+opts.DataDir)
	}
	if opts.Boundary != nil {
		c.cmd.Env = append(c.cmd.Env, "TMPDIR="+opts.Boundary.TempDir)
	}
	c.cmd.Dir = opts.Cwd
	var err error
	if c.stdin, err = c.cmd.StdinPipe(); err != nil {
		return nil, fmt.Errorf("minimax-acp: stdin pipe: %w", err)
	}
	// stdout is the protocol channel and MUST NOT be combined with stderr.
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("minimax-acp: stdout pipe: %w", err)
	}
	// stderr goes to a file, so protocol noise on stderr can never be mistaken for
	// a frame and so a crash leaves a retrievable diagnostic.
	ef, err := os.CreateTemp("", "minimax-acp-stderr-*.log")
	if err != nil {
		return nil, fmt.Errorf("minimax-acp: stderr capture: %w", err)
	}
	c.stderr = ef
	c.cmd.Stderr = ef
	// Its own process group, so shutdown can reach anything the server started.
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := c.cmd.Start(); err != nil {
		_ = ef.Close()
		return nil, fmt.Errorf("minimax-acp: starting %s: %w", opts.Launcher, err)
	}
	// The process now exists. Record it before anything else can fail, so a crash
	// from here on leaves something reapable. If recording fails, the child is
	// killed here rather than returned: an unrecorded live process is the exact
	// outcome this callback exists to prevent.
	if opts.OnSpawned != nil {
		if serr := opts.OnSpawned(c.cmd.Process.Pid); serr != nil {
			cleanupErr := abortSpawn(c.cmd, c.opts.ShutdownGrace)
			_ = ef.Close()
			return nil, fmt.Errorf("minimax-acp: the spawned process was not recorded: %w (cleanup: %v)",
				serr, cleanupErr)
		}
	}

	c.w = bufio.NewWriterSize(c.stdin, 1<<16)
	c.msgs = make(chan frame, MaxQueueDepth)
	go c.read(out)
	go c.pump()

	if err := c.handshake(ctx); err != nil {
		// A failed handshake is a TERMINATION like any other, so it goes through
		// the same cleanup path as a cancellation or a shutdown. It used to call a
		// narrower stop that reported success without proving the process was
		// gone, which meant a server that rejected `initialize` could leave a
		// live child behind an apparently clean startup failure.
		res := c.Teardown(ctx)
		return nil, fmt.Errorf("%w (cleanup: %s)", err, describeTeardown(res))
	}
	return c, nil
}

// read owns stdout for the life of the process.
//
// One reader, always. A per-call reader abandons goroutines on timeout, and an
// abandoned reader keeps consuming frames that a later call then misses — which
// presents as an intermittent protocol error rather than as the leak it is.
func (c *Connection) read(r io.Reader) {
	defer close(c.msgs)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), MaxFrameBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var f frame
		if json.Unmarshal(line, &f) != nil {
			// Non-JSON on stdout is a framing violation. It is ignored rather
			// than guessed at, because a guessed frame is an invented approval.
			continue
		}
		select {
		case c.msgs <- f:
		case <-c.closed:
			return
		}
	}
	c.mu.Lock()
	c.closeErr = sc.Err()
	c.mu.Unlock()
	close(c.closed)
}

// pump routes frames to waiters and to the observation stream.
func (c *Connection) pump() {
	for f := range c.msgs {
		switch {
		case f.ID != nil:
			c.mu.Lock()
			ch, ok := c.pending[*f.ID]
			if ok {
				delete(c.pending, *f.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- f
			}
		case f.Method != "":
			c.observe(f)
		}
	}
}

// observe forwards a server-initiated frame to the observation stream.
//
// Updates are DROPPED rather than blocking when nothing is consuming them, because
// a blocked reader would stall the very frames a decision is waiting on. The
// bounded channel is what keeps that from becoming unbounded memory.
func (c *Connection) observe(f frame) {
	if f.Method == "session/update" {
		var u struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(f.Params, &u) == nil && u.Update.Kind == "agent_message_chunk" && u.Update.Content.Type == "text" {
			c.mu.Lock()
			if c.activePrompt == u.SessionID {
				if len(c.promptText)+len(u.Update.Content.Text) > 1<<20 {
					c.promptTextOverflow = true
				} else {
					c.promptText += u.Update.Content.Text
				}
			}
			c.mu.Unlock()
		}
	}

	ev := runtime.Event{
		Backend: c.Name(),
		Cursor:  c.updateSeq.Add(1),
		Kind:    f.Method,
		Payload: f.Params,
	}
	var identity struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(f.Params, &identity) == nil {
		ev.SessionID = identity.SessionID
	}
	select {
	case c.updates <- ev:
	default: // the consumer is behind; drop rather than stall the protocol
		c.mu.Lock()
		c.droppedUpdates++
		c.mu.Unlock()
		c.markDegraded()
	}
}

// call sends a request and waits for its correlated response.
func (c *Connection) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	c.seq++
	id := c.seq
	ch := make(chan frame, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return nil, fmt.Errorf("minimax-acp: the server ended before answering %s", method)
			}
			if f.Error != nil {
				return nil, f.Error
			}
			return f.Result, nil
		case <-c.closed:
			c.mu.Lock()
			werr := c.closeErr
			c.mu.Unlock()
			return nil, fmt.Errorf("minimax-acp: the connection closed while waiting for %s: %v", method, werr)
		case <-timer.C:
			return nil, fmt.Errorf("minimax-acp: %s did not answer within %s", method, timeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// notify sends a notification: no id, no response.
//
// `session/cancel` is a notification. Sending it as a request yields
// "method not found", which is indistinguishable from the method being absent —
// a trap that makes a working cancellation look broken.
func (c *Connection) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// write serializes a frame.
//
// Writes are mutually exclusive because a half-written JSON line is unparseable
// and would desynchronise the stream for every later frame.
func (c *Connection) write(v map[string]any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("minimax-acp: encoding %v: %w", v["method"], err)
	}
	if len(line) > MaxFrameBytes {
		return fmt.Errorf("minimax-acp: a %v frame of %d bytes exceeds the %d byte limit",
			v["method"], len(line), MaxFrameBytes)
	}
	if _, err := c.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("minimax-acp: writing %v: %w", v["method"], err)
	}
	return c.w.Flush()
}

// handshake performs initialize and records the advertised capabilities.
func (c *Connection) handshake(ctx context.Context) error {
	// No client capabilities are advertised. The server grants filesystem and
	// terminal surfaces to the CLIENT; an unimplemented handler here would be one
	// the server can call, and it would fail open.
	raw, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}, c.opts.HandshakeTimeout)
	if err != nil {
		return fmt.Errorf("minimax-acp: initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("minimax-acp: decoding initialize: %w", err)
	}
	if res.ProtocolVersion == 0 {
		return errors.New("minimax-acp: the server did not report a protocol version")
	}
	c.caps.ProtocolVersion = res.ProtocolVersion
	c.agent = res.AgentInfo

	// sessionCapabilities are an OBJECT of empty objects, so each key's presence is
	// the signal. Decoding into a map avoids assuming a shape the server may vary.
	var caps struct {
		LoadSession bool                       `json:"loadSession"`
		MCP         map[string]bool            `json:"mcpCapabilities"`
		Prompt      map[string]bool            `json:"promptCapabilities"`
		SessionCaps map[string]json.RawMessage `json:"sessionCapabilities"`
	}
	_ = json.Unmarshal(res.AgentCaps, &caps)
	c.caps.LoadSession = caps.LoadSession
	c.caps.MCPHTTP = caps.MCP["http"]
	c.caps.MCPStream = caps.MCP["sse"]
	c.caps.PromptImage = caps.Prompt["image"]
	c.caps.PromptAudio = caps.Prompt["audio"]
	c.caps.PromptEmbeddedCtx = caps.Prompt["embeddedContext"]
	_, c.caps.SessionList = caps.SessionCaps["list"]
	_, c.caps.SessionResume = caps.SessionCaps["resume"]
	_, c.caps.SessionClose = caps.SessionCaps["close"]
	c.negociated = true
	// Replay receipts that were made durable but never acknowledged. The consumer
	// deduplicates on send id, so a redelivery is harmless and a lost one is not.
	if derr := c.drainSpool(); derr != nil {
		c.markDegraded()
	}
	return nil
}

// Capabilities reports what the connected server advertises.
func (c *Connection) Capabilities() runtime.Capabilities {
	// Inventory is deliberately FALSE. This backend owns exactly the process it
	// spawned and cannot list anything else, so it must not claim to reconcile by
	// inventory — it reconciles by committed activation intent instead.
	return runtime.Capabilities{
		Inventory: false,
		Output:    false, // ACP has no "read the last response" operation
		Receipts:  true,  // correlated JSON-RPC responses are receipts
		Resume:    c.caps.LoadSession && c.caps.SessionResume,
		Close:     c.caps.SessionClose,
		// DurableQueue is FALSE and must stay false. A prompt is written straight
		// to a JSON-RPC peer; once this process dies the server retains nothing we
		// can ask about, so an unknown outcome cannot be resolved by reconciling.
		// Declaring a queue here would authorise exactly the automatic replay that
		// would duplicate a prompt.
		DurableQueue: false,
	}
}

// Name identifies the transport.
func (c *Connection) Name() string {
	if c.opts.BackendID != "" {
		return c.opts.BackendID
	}
	return BackendName
}

// Agent returns the negotiated server identity, for provenance.
func (c *Connection) Agent() AgentInfo { return c.agent }

// verifyLauncher checks the pinned identity before spawning.
func verifyLauncher(opts Options) error {
	if strings.TrimSpace(opts.Launcher) == "" {
		return errors.New("minimax-acp: no launcher is configured")
	}
	if !strings.HasPrefix(opts.Launcher, "/") {
		// A bare name would resolve through PATH, which is exactly how a reviewed
		// client ends up executing something else.
		return fmt.Errorf("minimax-acp: the launcher must be an absolute path, got %q", opts.Launcher)
	}
	if strings.TrimSpace(opts.LauncherSHA256) == "" {
		return errors.New("minimax-acp: the launcher has no approved digest; an unpinned launcher is refused")
	}
	got, err := runtime.FileSHA256(opts.Launcher)
	if err != nil {
		return fmt.Errorf("minimax-acp: the launcher cannot be measured: %w", err)
	}
	if !strings.EqualFold(got, opts.LauncherSHA256) {
		return fmt.Errorf("minimax-acp: the launcher hashes to %s but %s was approved; it has been replaced",
			got, opts.LauncherSHA256)
	}
	return nil
}

// --- runtime.Backend -------------------------------------------------------

// Session is one ACP session's negotiated state.
type Session struct {
	ID          string
	Options     []ConfigOption
	CurrentMode string
	Modes       []Mode
}

// ConfigOption is one selectable configuration value the server advertises.
type ConfigOption struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	CurrentValue string   `json:"currentValue"`
	Options      []string `json:"-"`
}

// ValueKnown reports whether a value is one the server advertised.
//
// Refusing an unadvertised value is the whole point: a typo that silently fell
// back to a default would mean the admitted model is not the running model.
func (c ConfigOption) ValueKnown(v string) bool {
	for _, o := range c.Options {
		if o == v {
			return true
		}
	}
	return false
}

// Mode is a named session mode.
type Mode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type sessionResult struct {
	SessionID string `json:"sessionId"`
	Modes     struct {
		CurrentModeID  string `json:"currentModeId"`
		AvailableModes []Mode `json:"availableModes"`
	} `json:"modes"`
	ConfigOptions []struct {
		Type         string `json:"type"`
		ID           string `json:"id"`
		CurrentValue string `json:"currentValue"`
		Options      []struct {
			Value string `json:"value"`
			Name  string `json:"name"`
		} `json:"options"`
	} `json:"configOptions"`
}

// Activate creates a session and applies the FROZEN selections.
//
// The order is deliberate: create, then apply, then verify. Applying before
// verifying would let a rejected selection leave the session running on the
// server's default while the caller believes the admitted model is active.
// Activate creates a session with the frozen selections and returns its reference.
//
// The negotiated Session is retained for Shutdown, so a session can be closed
// rather than left for a process kill to reap.
func (c *Connection) Activate(ctx context.Context, spec runtime.ActivateSpec) (runtime.SessionRef, error) {
	ref, _, err := c.ActivateResult(ctx, spec)
	return ref, err
}

// applySelection freezes model and permission mode, refusing anything the server
// did not advertise.
func (c *Connection) applySelection(ctx context.Context, sess *Session, model, permission string) error {
	for _, want := range []struct{ id, value string }{
		{"permissionMode", permission},
		{"model", model},
		{"thinkingEffort", c.opts.Effort},
	} {
		if strings.TrimSpace(want.value) == "" {
			// Unset means "record whatever the server chose", not "choose for it".
			continue
		}
		opt := sess.Option(want.id)
		if opt == nil {
			return Unsupported(want.id)
		}
		if !opt.ValueKnown(want.value) {
			// Refused rather than coerced. Falling back to a default here would
			// mean the worker runs on a model nobody admitted.
			return fmt.Errorf("minimax-acp: %s value %q is not one the server advertises (%s)",
				want.id, want.value, strings.Join(opt.Options, ", "))
		}
		raw, err := c.call(ctx, "session/set_config_option", map[string]any{
			"sessionId": sess.ID, "configId": want.id, "value": want.value,
		}, c.opts.CallTimeout)
		if err != nil {
			return fmt.Errorf("minimax-acp: selecting %s=%s: %w", want.id, want.value, err)
		}
		// Read back rather than trusting the echo. A set_config_option reply
		// carries configOptions WITHOUT a sessionId, so it is parsed as an
		// option set rather than as a session — conflating the two shapes made
		// every legitimate selection look like a corrupt reply.
		after, perr := parseConfigOptions(raw)
		if perr != nil {
			return fmt.Errorf("minimax-acp: reading back %s: %w", want.id, perr)
		}
		if got := after.Option(want.id); got == nil || got.CurrentValue != want.value {
			return fmt.Errorf("minimax-acp: %s was set to %q but the server reports %q",
				want.id, want.value, currentOf(after, want.id))
		}
		// Model selection can advertise additional controls such as effort.
		sess.Options = after.Options
	}
	return nil
}

func currentOf(s *Session, id string) string {
	if o := s.Option(id); o != nil {
		return o.CurrentValue
	}
	return "(absent)"
}

// Option returns a session's advertised configuration option.
func (s *Session) Option(id string) *ConfigOption {
	for i := range s.Options {
		if s.Options[i].ID == id {
			return &s.Options[i]
		}
	}
	return nil
}

func (s *Session) setCurrent(id, v string) {
	if o := s.Option(id); o != nil {
		o.CurrentValue = v
	}
}

// parseConfigOptions reads a reply that carries only the option set.
func parseConfigOptions(raw json.RawMessage) (*Session, error) {
	var body struct {
		ConfigOptions []struct {
			Type         string `json:"type"`
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
			Options      []struct {
				Value string `json:"value"`
			} `json:"options"`
		} `json:"configOptions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("minimax-acp: decoding config options: %w", err)
	}
	s := &Session{}
	for _, co := range body.ConfigOptions {
		opt := ConfigOption{ID: co.ID, Type: co.Type, CurrentValue: co.CurrentValue}
		for _, o := range co.Options {
			opt.Options = append(opt.Options, o.Value)
		}
		s.Options = append(s.Options, opt)
	}
	return s, nil
}

func parseSession(raw json.RawMessage) (*Session, error) {
	var sr sessionResult
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("minimax-acp: decoding the session: %w", err)
	}
	if strings.TrimSpace(sr.SessionID) == "" {
		return nil, errors.New("minimax-acp: the server returned a session with no id")
	}
	s := &Session{ID: sr.SessionID, CurrentMode: sr.Modes.CurrentModeID, Modes: sr.Modes.AvailableModes}
	for _, co := range sr.ConfigOptions {
		opt := ConfigOption{ID: co.ID, Type: co.Type, CurrentValue: co.CurrentValue}
		for _, o := range co.Options {
			opt.Options = append(opt.Options, o.Value)
		}
		s.Options = append(s.Options, opt)
	}
	return s, nil
}

// SetMode requests a mode change and reports when it takes effect.
//
// The installed server answers `transition: next_prompt`, meaning a mode change
// cannot interrupt a running turn. That is a scheduling fact the caller must plan
// around, not an error to retry.
func (c *Connection) SetMode(ctx context.Context, sessionID, modeID string) (string, error) {
	raw, err := c.call(ctx, "session/set_mode", map[string]any{
		"sessionId": sessionID, "modeId": modeID,
	}, c.opts.CallTimeout)
	if err != nil {
		return "", classifyAuth(err)
	}
	var meta struct {
		Meta struct {
			Transition string `json:"minimax-code/transition"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.Meta.Transition, nil
}

// Cancel asks the server to stop the active turn.
//
// It is a NOTIFICATION: there is no response to wait for, so this returns as soon
// as the frame is written. That is a real limitation, not a formality — a
// notification offers no acknowledgement, so a caller must observe the turn's
// outcome through updates rather than assuming cancellation took effect.
func (c *Connection) Cancel(ctx context.Context, sessionID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := c.notify("session/cancel", map[string]any{"sessionId": sessionID}); err != nil {
		return err
	}
	// The frame is written, but notification delivery is unacknowledged. Give the
	// server a bounded moment to act, then let the caller observe.
	time.Sleep(50 * time.Millisecond)
	return nil
}

// Updates exposes the observation stream.
func (c *Connection) Updates() <-chan runtime.Event { return c.updates }

// spoolReceipt appends one receipt to the durable spool and fsyncs it.
//
// It is deliberately append-only: a receipt is an immutable statement about one
// turn, and rewriting history is what would let a lost completion quietly
// disappear.
func (c *Connection) spoolReceipt(body []byte) error {
	c.spoolMu.Lock()
	defer c.spoolMu.Unlock()
	if c.spool == nil {
		return errors.New("minimax-acp: no receipt spool is configured, so a completion cannot be made durable")
	}
	line := append(body, '\n')
	if _, err := c.spool.Write(line); err != nil {
		return err
	}
	return c.spool.Sync()
}

// drainSpool replays receipts that were durably recorded but never acknowledged.
//
// Acknowledgement, not publication, is what retires a receipt: publication hands
// it to a live consumer, and a consumer that dies before acting must still find
// the receipt on the next start.
func (c *Connection) drainSpool() error {
	if c.spoolPath == "" {
		return nil
	}
	f, err := os.Open(c.spoolPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing was ever spooled
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxFrameBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var body struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal([]byte(line), &body); err != nil {
			// A torn tail is the expected shape of a crash mid-append. Everything
			// before it is still valid, so it is skipped rather than discarded.
			continue
		}
		c.mu.Lock()
		seen := c.ackedReceipts[body.SessionID+"|"+digestOf(line)]
		c.mu.Unlock()
		if seen {
			continue
		}
		select {
		case c.receipts <- runtime.Event{
			Backend: c.Name(), Kind: runtime.KindPromptResult,
			SessionID: body.SessionID, Payload: []byte(line),
		}:
		default:
		}
	}
	return sc.Err()
}

// AcknowledgeReceipt retires a receipt once its consumer has durably recorded it.
//
// It is the acknowledgement half of the durable handoff. Truncating the spool
// here — rather than at publication — is what makes the delivery at-least-once,
// and the consumer's own deduplication is what makes it effectively-once.
func (c *Connection) AcknowledgeReceipt(sessionID, sendID string) error {
	c.mu.Lock()
	if c.ackedReceipts == nil {
		c.ackedReceipts = map[string]bool{}
	}
	c.ackedReceipts[sessionID+"|"+sendID] = true
	c.mu.Unlock()
	return c.compactSpool()
}

// compactSpool drops receipts that have been acknowledged.
//
// It rewrites the file and swaps it, so a crash mid-compaction leaves either the
// old complete file or the new one — never a half-written one that would lose an
// unacknowledged receipt.
func (c *Connection) compactSpool() error {
	c.spoolMu.Lock()
	defer c.spoolMu.Unlock()
	if c.spool == nil || c.spoolPath == "" {
		return nil
	}
	f, err := os.Open(c.spoolPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var keep []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var body struct {
			SessionID string `json:"sessionId"`
			SendID    string `json:"sendId"`
		}
		if json.Unmarshal([]byte(line), &body) != nil {
			continue
		}
		c.mu.Lock()
		acked := c.ackedReceipts[body.SessionID+"|"+body.SendID]
		c.mu.Unlock()
		if !acked {
			keep = append(keep, line)
		}
	}
	f.Close()

	tmp := c.spoolPath + ".compact"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, line := range keep {
		if _, err := out.WriteString(line + "\n"); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	out.Close()
	if err := os.Rename(tmp, c.spoolPath); err != nil {
		return err
	}
	return c.spool.Close()
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// StopOwnedProcesses terminates every process this connection owns.
//
// It exists because Shutdown closes ONE SESSION, which is not the same as
// ending the work. A transport that owns a child process must be able to prove
// that process is gone: closing a session can fail while the process keeps
// writing, and a cancellation that only closed sessions would report success
// over a workspace still being modified.
//
// It is a NAME for the one teardown path rather than a second implementation of
// it, so a caller can ask for the capability without knowing the method.
func (c *Connection) StopOwnedProcesses(ctx context.Context) error {
	return c.Teardown(ctx).Err()
}

// UnreapedProcesses reports descendants that survived this connection's teardown.
//
// It is a query the control layer asks rather than a verdict it infers. A tool
// that left the process group cannot be reached by the group signal, so the only
// honest thing to do with one is name it; a control layer that computed this
// itself would have to guess, and a guess here is either a hidden survivor or a
// false failure.
func (c *Connection) UnreapedProcesses() []int {
	c.teardownOnce.Do(func() {
		c.teardownRes = c.runTeardown(context.Background())
		close(c.teardownDone)
	})
	<-c.teardownDone
	return c.teardownRes.Unreaped
}

// describeTeardown renders a teardown verdict for an operator-facing error.
//
// A failure is spelled as a failure. Collapsing it into "clean" here would undo
// the entire reason the result is a distinct type.
func describeTeardown(res TeardownResult) string {
	switch {
	case len(res.Unreaped) > 0:
		return fmt.Sprintf("NOT CLEAN: descendant process(es) %v survived and are NOT proven reaped", res.Unreaped)
	case res.Clean():
		if len(res.Unreaped) > 0 {
			return fmt.Sprintf("%s, but descendant process(es) %v survived and are NOT proven reaped",
				res.Outcome, res.Unreaped)
		}
		return res.Outcome
	default:
		return "NOT CLEAN: " + res.Outcome + ": " + res.Detail
	}
}

// Receipts is the correlated-receipt stream, separate from notifications.
//
// A consumer that wants to know when work finished follows THIS, because it is
// the only stream that carries the event, and it is not shareable with the
// high-volume notification traffic.
func (c *Connection) Receipts() <-chan runtime.Event { return c.receipts }

// DroppedReceipts reports receipts lost to a full receipt stream.
//
// It is a count rather than a bool because the operator needs to know whether
// this has ever happened, not just that it is happening now.
func (c *Connection) DroppedReceipts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.droppedReceipts
}

// markDegraded records that this connection can no longer be trusted to deliver
// every completion.
//
// Dropping a receipt is not a recoverable condition for the supervisor: there is
// no second copy to reconcile against, so the honest response is to say the
// transport is degraded rather than to keep looking healthy.
func (c *Connection) markDegraded() {
	c.mu.Lock()
	c.degraded = true
	c.mu.Unlock()
}

// Shutdown closes ONE session, satisfying the boundary.
//
// It does not stop the process. The boundary's shutdown is per-session so a
// transport can retire one session without disturbing another; connection-level
// teardown is Teardown, and Stop is its legacy name.
//
// The session is recorded as CLOSED here. A session that was closed on purpose
// and one that vanished when the connection died are both "not open", and only
// the record tells them apart.
func (c *Connection) Shutdown(ctx context.Context, ref runtime.SessionRef) error {
	grace := c.opts.ShutdownGrace
	gctx, cancel := runtime.Deadline(ctx, grace)
	defer cancel()
	if _, err := c.call(gctx, "session/close", map[string]any{"sessionId": ref.ID}, grace); err != nil {
		return err
	}
	c.markSessionClosed(ref.ID)
	return nil
}

// Stop closes every open session and then stops the owned process.
//
// It is a NAME for the one teardown path. It used to be a second implementation
// of termination, and it differed from the spawn-abort path in exactly the way
// that matters: it returned "killed" after signalling without ever checking
// whether the process had gone, and it waited unbounded on the reap after
// SIGKILL. A wedged child could therefore hang a shutdown that is required to be
// bounded, and a surviving child could be reported as killed.
func (c *Connection) Stop(ctx context.Context) (string, error) {
	res := c.Teardown(ctx)
	return res.Outcome, res.Err()
}

// classifyAuth distinguishes an authentication failure from a protocol one.
//
// They demand opposite responses — wait for a login versus refuse the
// configuration — so conflating them would have a caller retrying a bad request
// forever or giving up on a fixable account state.
func classifyAuth(err error) error {
	var re *rpcError
	if errors.As(err, &re) && re.Code == -32000 {
		return fmt.Errorf("%w: %s", ErrNotAuthenticated, re.Message)
	}
	return err
}

// ActivateResult creates a session and returns both its reference and its
// negotiated configuration, which is what an admission record must freeze.
func (c *Connection) ActivateResult(ctx context.Context, spec runtime.ActivateSpec) (runtime.SessionRef, *Session, error) {
	if strings.TrimSpace(spec.Label) == "" {
		return runtime.SessionRef{}, nil, fmt.Errorf("%w: an activation label is required so an ambiguous "+
			"create can be reconciled", runtime.ErrUnsupported)
	}
	raw, err := c.call(ctx, "session/new", map[string]any{
		"cwd": c.opts.Cwd, "mcpServers": []any{},
	}, c.opts.CallTimeout)
	if err != nil {
		return runtime.SessionRef{}, nil, classifyAuth(err)
	}
	sess, err := parseSession(raw)
	if err != nil {
		return runtime.SessionRef{}, nil, err
	}
	// Track creation before selection: even a refused or timed-out selector
	// leaves a session that teardown must close.
	c.mu.Lock()
	c.openSessions[sess.ID] = true
	c.mu.Unlock()
	if err := c.applySelection(ctx, sess, c.opts.Model, c.opts.PermissionMode); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), c.opts.CallTimeout)
		defer cancel()
		_, closeErr := c.call(cleanupCtx, "session/close", map[string]any{"sessionId": sess.ID}, c.opts.CallTimeout)
		if closeErr != nil {
			return runtime.SessionRef{}, nil, errors.Join(err, fmt.Errorf("closing refused session %s: %w", sess.ID, closeErr))
		}
		c.mu.Lock()
		delete(c.openSessions, sess.ID)
		c.closedSessions[sess.ID] = true
		c.mu.Unlock()
		return runtime.SessionRef{}, nil, err
	}
	c.mu.Lock()
	if spec.Label != "" {
		c.openLabels[spec.Label] = sess.ID
	}
	c.mu.Unlock()
	return runtime.SessionRef{Backend: c.Name(), ID: sess.ID, Label: spec.Label}, sess, nil
}

// Reconcile finds an existing session for a label.
//
// It CANNOT use inventory, because this backend owns exactly one process and
// cannot list anything else. Reconciliation is therefore by committed activation
// intent, and the honest answer for an unknown label is "cannot tell" rather than
// "absent" — which is why this returns an error instead of found=false.
func (c *Connection) Reconcile(_ context.Context, label string, priorQuiesced bool) (runtime.SessionRef, bool, error) {
	c.mu.Lock()
	if id, ok := c.openLabels[label]; ok {
		c.mu.Unlock()
		return runtime.SessionRef{Backend: c.Name(), ID: id, Label: label}, true, nil
	}
	c.mu.Unlock()

	// This process tracks every session it created, so "not in my map" is a
	// definite answer about THIS process. It says nothing about an orphan from an
	// earlier run, and an orphan holding this label is exactly how a second
	// worker appears — so without the caller's proof of quiescence the honest
	// answer is that we cannot tell.
	if !priorQuiesced {
		return runtime.SessionRef{}, false, fmt.Errorf("%w: no session carries label %q on this process, but a "+
			"process from an earlier run has not been proven gone, so whether the label is unused cannot be "+
			"determined here", runtime.ErrUnsupported, label)
	}
	return runtime.SessionRef{Backend: c.Name(), ID: ""}, false, nil
}

// ProcessIdentity reports the owned child's process id.
//
// It exists so an activation can record WHICH process a session belongs to. A
// session id alone cannot answer "is the thing that made this still running?",
// and answering that wrongly in either direction is expensive: assume-dead
// creates a second worker, assume-alive strands one.
func (c *Connection) ProcessIdentity() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return 0, false
	}
	return c.cmd.Process.Pid, true
}

// Inspect reports the owned process's liveness.
//
// There is no per-session inspect: the child process IS the session's execution,
// so a running process is the only honest answer available without spending a
// model call to ask.
//
// It does not collapse CLOSED into UNKNOWN. A session this connection closed on
// purpose has finished; one that vanished with the connection should have been
// running. Reporting both as "unknown" is what forces a supervisor to choose
// between deleting a finished session and re-dispatching to a dead one, and it
// guesses in whichever direction is cheapest — which is how a stale session gets
// treated as though it were done.
func (c *Connection) Inspect(_ context.Context, ref runtime.SessionRef) (runtime.Session, error) {
	switch c.SessionLiveness(ref) {
	case SessionActive:
		return runtime.Session{ID: ref.ID, Tool: BackendName, Status: "running"}, nil
	case SessionClosed:
		// A terminal status, not an error. The session is done and nothing is owed
		// to it; a caller that re-dispatches to it is the caller's bug to fix, not
		// something this layer should disguise as a missing session.
		return runtime.Session{ID: ref.ID, Tool: BackendName, Status: "closed"}, nil
	case SessionStale:
		return runtime.Session{ID: ref.ID, Tool: BackendName, Status: "stale"},
			fmt.Errorf("%w: session %s should be running but its connection ended without closing it",
				runtime.ErrUnknownSession, ref.ID)
	default:
		return runtime.Session{ID: ref.ID},
			fmt.Errorf("%w: %s is not a session this connection knows", runtime.ErrUnknownSession, ref.ID)
	}
}

// DeliverGuidance sends a queued instruction to an idle session.
//
// It exists because the control layer needs to know whether an instruction
// actually ARRIVED. Queueing it is not delivery, and treating a queued item as
// delivered is precisely the failure this API is meant to rule out: the worker
// would be believed informed while nothing was sent.
//
// It reuses Submit, so the one-prompt-at-a-time guard still applies and a
// guidance sent at a busy boundary fails loudly rather than interleaving.
func (c *Connection) DeliverGuidance(ctx context.Context, ref runtime.SessionRef, message string) (runtime.Receipt, error) {
	return c.Submit(ctx, ref, message)
}

// Submit sends one prompt.
//
// This is the paid operation. Exactly one may be in flight per connection, and a
// second concurrent prompt is refused rather than queued behind an unknown
// turn boundary.
func (c *Connection) Submit(ctx context.Context, ref runtime.SessionRef, message string) (runtime.Receipt, error) {
	c.mu.Lock()
	busy := c.activePrompt != ""
	if busy {
		c.mu.Unlock()
		return runtime.Receipt{}, fmt.Errorf("%w: a prompt is already active on this session; a coding "+
			"agent's turn does not tolerate concurrent prompts", runtime.ErrUnsupported)
	}
	c.activePrompt = ref.ID
	c.promptText = ""
	c.promptTextOverflow = false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.activePrompt = ""
		c.mu.Unlock()
	}()

	raw, err := c.call(ctx, "session/prompt", map[string]any{
		"sessionId": ref.ID,
		"prompt":    []any{map[string]any{"type": "text", "text": message}},
	}, c.opts.PromptTimeout())
	if err != nil {
		// No receipt is returned, and that is the point. The prompt may or may not
		// have reached the peer before the failure, so the caller gets an unknown
		// outcome and must block rather than retry.
		return runtime.Receipt{}, classifyAuth(err)
	}
	var res struct {
		StopReason string `json:"stopReason"`
		Model      string `json:"model"`
	}
	_ = json.Unmarshal(raw, &res)
	// The stop reason is recorded verbatim. It is NOT an engineering outcome: a
	// completed prompt proves the agent processed it, not that the work passed.
	//
	// OutcomeLanded is used because the JSON-RPC request/response pair is the
	// transport's own durable receipt: this call returned an answer, so the
	// prompt demonstrably reached the peer and the turn ended.
	sendID := c.newSendID(ref.ID)
	c.mu.Lock()
	text, overflow := c.promptText, c.promptTextOverflow
	c.mu.Unlock()
	c.publishTurnReceipt(ref.ID, sendID, res.StopReason, text, res.Model, overflow)

	return runtime.Receipt{
		SendID:  sendID,
		State:   res.StopReason,
		Settled: true,
		Outcome: runtime.OutcomeLanded,
	}, nil
}

// publishReceipt puts the CORRELATED terminal receipt onto the observation
// stream.
//
// Without this the receipt exists only as a return value: a JSON-RPC response is
// answered to the caller that requested it, so a supervisor following the
// notification stream would never see that the turn ended. That is not a
// convenience gap — it means the ACP worker has no production route to
// completion, and it would look healthy while never being assessed.
//
// The receipt is published BEFORE it is returned, so a crash after the turn
// cannot lose the record of it.
func (c *Connection) publishReceipt(sessionID, sendID, stopReason string) {
	c.publishTurnReceipt(sessionID, sendID, stopReason, "", "", false)
}

func (c *Connection) publishTurnReceipt(sessionID, sendID, stopReason, text, model string, truncated bool) {
	body, err := json.Marshal(map[string]any{
		"sessionId":  sessionID,
		"sendId":     sendID,
		"stopReason": stopReason,
		"text":       text,
		"model":      model,
		"truncated":  truncated,
	})
	if err != nil {
		return
	}
	// The receipt is made DURABLE before it is published.
	//
	// A receipt held only in memory is lost when the process dies, and a lost
	// receipt is a completion that will never be assessed — which presents as a
	// healthy supervisor watching an agent it will never judge.
	//
	// If the spool write fails the receipt is NOT published at all: reporting a
	// completion we cannot guarantee is exactly how an episode silently loses a
	// phase. The connection degrades instead.
	if serr := c.spoolReceipt(body); serr != nil {
		c.mu.Lock()
		c.droppedReceipts++
		c.mu.Unlock()
		c.markDegraded()
		return
	}
	ev := runtime.Event{
		Backend: c.Name(), Kind: runtime.KindPromptResult,
		SessionID: sessionID, Payload: body,
	}
	select {
	case c.receipts <- ev:
	default:
		// The receipt stream is full. It is NOT dropped quietly: an unrecorded
		// receipt is a completion that will silently never be assessed, so the
		// connection is marked unhealthy and the count is kept for the operator.
		c.mu.Lock()
		c.droppedReceipts++
		c.mu.Unlock()
		c.markDegraded()
	}
}

// newSendID mints a distinct receipt id per submission.
//
// The session id alone is not an identity: two prompts to the same session would
// produce the same id, and a reconciliation that cannot tell two deliveries
// apart cannot say which one it is answering.
func (c *Connection) newSendID(sessionID string) string {
	n := c.sendSeq.Add(1)
	return fmt.Sprintf("acp:%s:%d", sessionID, n)
}

// Receipt reconciles a previously submitted prompt.
//
// JSON-RPC answers its own request, so a receipt is only outstanding while a call
// is in flight. A completed submission is settled by construction, and the id
// itself is proof of that: it is minted only after an answer arrives, so a
// recorded id means the peer answered rather than that the prompt was merely
// attempted.
func (c *Connection) Receipt(_ context.Context, sendID string) (runtime.Receipt, error) {
	if !strings.HasPrefix(sendID, "acp:") {
		return runtime.Receipt{}, fmt.Errorf("%w: %q is not an ACP receipt", runtime.ErrUnsupported, sendID)
	}
	return runtime.Receipt{SendID: sendID, State: "answered", Settled: true, Outcome: runtime.OutcomeLanded}, nil
}

// Output is not available on this transport.
//
// ACP exposes no "read the last response" operation, so claiming otherwise would
// make the broker's review ingress reach for something that does not exist.
func (c *Connection) Output(_ context.Context, _ runtime.SessionRef, _ int) (string, bool, error) {
	return "", false, Unsupported("session output")
}

// Events follows the connection's observation stream.
func (c *Connection) Events(ctx context.Context, after uint64) (<-chan runtime.Event, <-chan error, error) {
	// Notification history is not durable and cannot be replayed by this
	// connection. Never silently treat a saved position as a fresh subscription.
	if after != 0 {
		return nil, nil, Unsupported("notification replay after a saved cursor")
	}
	errs := make(chan error, 1)
	out := make(chan runtime.Event, MaxQueueDepth)
	go func() {
		defer close(out)
		defer close(errs)
		for {
			select {
			case ev, ok := <-c.updates:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case <-c.closed:
				errs <- c.closeErrLocked()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errs, nil
}

func (c *Connection) closeErrLocked() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Health reports connection state rather than a global bus flag.
//
// A single shared "bus healthy" would be true for a connection that is alive but
// has never observed anything, which is exactly the failure that looks healthy.
func (c *Connection) Health(_ context.Context) (runtime.Health, error) {
	select {
	case <-c.closed:
		return runtime.Health{Available: false, Reasons: []string{"connection-closed"}}, nil
	default:
	}
	c.mu.Lock()
	dropped := c.droppedReceipts
	updatesDropped := c.droppedUpdates
	c.mu.Unlock()
	if updatesDropped > 0 {
		return runtime.Health{Available: false, Reasons: []string{"notifications-dropped", "the ACP notification stream lost observations"}}, nil
	}
	if dropped > 0 {
		// A lost receipt cannot be reconciled: there is no second copy. Reporting
		// this connection healthy while a completion has gone missing is how a
		// supervisor comes to believe nothing needed assessing.
		return runtime.Health{
			Available: false,
			Reasons:   []string{"receipts-dropped", "a completion receipt was lost; it cannot be reconciled"},
		}, nil
	}
	// A teardown that did not establish quiescence is surfaced rather than
	// swallowed. It is reported by OUTCOME rather than by returning an error,
	// because Health is a description of the connection, not an operation on it —
	// but it must never read as available, or the operator is told everything is
	// fine over a process that may still be holding the workspace.
	if res, ran := c.teardownVerdict(); ran && !res.Clean() && res.Outcome != TeardownNothing {
		return runtime.Health{
			Available: false,
			Reasons: []string{"cleanup-failed",
				"the owned process could not be proven gone: " + res.Detail},
		}, nil
	}
	return runtime.Health{Available: true}, nil
}

// teardownVerdict reads a completed teardown verdict without STARTING one.
//
// Health is a query. Calling Teardown from it would turn an operator's status
// check into a process termination, which is a side effect hidden inside a getter
// and would stop a worker that is still legitimately working.
func (c *Connection) teardownVerdict() (TeardownResult, bool) {
	select {
	case <-c.teardownDone:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.teardownRes, true
	default:
		return TeardownResult{}, false
	}
}

// PromptTimeout is the bound for one model call.
//
// It is long, because a real coding turn is long, and it is finite, because an
// unbounded wait is indistinguishable from a hung worker.
func (o Options) PromptTimeout() time.Duration {
	if o.TurnTimeout > 0 {
		return o.TurnTimeout
	}
	return DefaultTurnTimeout
}

// cancelTo is Cancel's write, factored so a test can observe the exact frame
// without a live turn.
//
// A notification has no response, so its correctness is entirely a property of
// what was written. Testing the public Cancel would prove nothing about the frame.
func (c *Connection) cancelTo(ctx context.Context, sessionID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return c.notify("session/cancel", map[string]any{"sessionId": sessionID})
}

// ProtocolVersion reports the negotiated protocol version, for provenance.
func (c *Connection) ProtocolVersion() int { return c.caps.ProtocolVersion }

// EnsureSessionRunning proves an ACP session is genuinely open.
//
// ACP sessions are live from creation, so there is no start step and pretending
// otherwise would report success for a session that does not exist.
func (c *Connection) EnsureSessionRunning(_ context.Context, ref runtime.SessionRef) error {
	select {
	case <-c.closed:
		return fmt.Errorf("%w: the ACP connection is closed", runtime.ErrUnknownSession)
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.openSessions[ref.ID] {
		return fmt.Errorf("%w: %s is not an open ACP session on this connection", runtime.ErrUnknownSession, ref.ID)
	}
	return nil
}
