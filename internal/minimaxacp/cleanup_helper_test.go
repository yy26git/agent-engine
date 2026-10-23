package minimaxacp

// The detached-descendant fixture.
//
// macOS ships no `setsid` binary and no `perl`, so a shell fixture cannot detach
// a grandchild the way a real tool does. Rather than pretend a same-group child is
// detached — which would prove nothing — the test binary itself stands in for the
// launcher and produces a genuinely out-of-group descendant with Setsid(2).
//
// This is a SUBPROCESS FIXTURE only. It never launches a model, never sends a
// prompt, and never reaches the network.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// TestMain lets this binary be its own ACP server fixture.
//
// The helper roles must not fall through into the test suite, or spawning one
// would recursively spawn more. The detacher is dispatched on the `acp` argument
// rather than on an environment variable, because `Start` execs the launcher as
// `<launcher> acp` and offers no way to hand that child an environment; the
// idler, which this process spawns itself, is dispatched on the environment.
func TestMain(m *testing.M) {
	if os.Getenv(cleanupRoleEnv) == roleIdler {
		runIdler()
		return
	}
	for _, arg := range os.Args[1:] {
		if arg == "acp" {
			runDetacher()
			return
		}
	}
	os.Exit(m.Run())
}

const (
	cleanupRoleEnv   = "AGENT_ENGINE_CLEANUP_ROLE"
	cleanupMarkerEnv = "AGENT_ENGINE_CLEANUP_MARKER"
	roleDetacher     = "detacher"
	roleIdler        = "idler"
	// helperDeadline bounds a fixture that never receives what it waits for, so
	// a broken fixture fails the test rather than hanging the suite.
	helperDeadline = 20 * time.Second
)

// runIdler is the DETACHED descendant.
//
// It leaves its parent's process group with Setsid(2), which is precisely the
// move that puts a tool outside the reach of the group signal a teardown sends.
// It then ignores every catchable signal and waits to be killed.
func runIdler() {
	marker := os.Getenv(cleanupMarkerEnv)
	if marker == "" {
		fmt.Fprintln(os.Stderr, "cleanup idler: no marker path")
		os.Exit(2)
	}
	// The marker IS the barrier: the detacher refuses to answer `initialize`
	// until this file exists, so no test needs to sleep and guess.
	if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup idler: write marker:", err)
		os.Exit(3)
	}
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	for {
		time.Sleep(time.Hour)
	}
}

// runDetacher stands in for the pinned launcher: it spawns one detached
// grandchild, waits for the barrier, and then answers the ACP handshake.
func runDetacher() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cleanup detacher: resolve self:", err)
		os.Exit(2)
	}
	child := exec.Command(self)
	child.Env = append(os.Environ(), cleanupRoleEnv+"="+roleIdler)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// stdio is detached from ours: a grandchild reading the protocol pipe would
	// steal frames and turn a fixture bug into a client bug.
	child.Stdin, child.Stdout, child.Stderr = nil, nil, nil
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup detacher: spawn detached grandchild:", err)
		os.Exit(3)
	}
	// The grandchild is deliberately NOT waited for: it is meant to outlive this
	// process, and waiting would make it a same-group child after all.
	_ = child.Process.Release()

	marker := os.Getenv(cleanupMarkerEnv)
	deadline := time.Now().Add(helperDeadline)
	for time.Now().Before(deadline) {
		if fi, serr := os.Stat(marker); serr == nil && fi.Size() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	serveHandshake()
}

// serveHandshake answers `initialize` and nothing else, over the real pipe.
func serveHandshake() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<16), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var f struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(line), &f) != nil || f.ID == nil {
			continue
		}
		if f.Method != "initialize" {
			continue
		}
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": *f.ID,
			"result": map[string]any{
				"protocolVersion":   1,
				"agentCapabilities": map[string]any{"loadSession": false, "sessionCapabilities": map[string]any{}},
				"agentInfo":         map[string]any{"name": "detacher", "version": "0"},
			},
		})
		out.Write(body)
		out.WriteByte('\n')
		out.Flush()
	}
}

// detachingConn starts the detacher fixture as the pinned launcher.
func detachingConn(t *testing.T) (*Connection, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve the test binary: %v", err)
	}
	sum, err := runtime.FileSHA256(self)
	if err != nil {
		t.Fatalf("measure the test binary: %v", err)
	}
	marker := t.TempDir() + "/grandchild.pid"
	t.Setenv(cleanupMarkerEnv, marker)

	c, err := Start(context.Background(), Options{
		Launcher: self, LauncherSHA256: sum, Cwd: t.TempDir(),
		HandshakeTimeout: 10 * time.Second, ShutdownGrace: cleanupGrace,
	})
	if err != nil {
		t.Fatalf("start the detaching fixture: %v", err)
	}
	return c, marker
}

// compile-time proof the fixture really detaches. Without this a future edit that
// drops Setsid would make the survivor tests pass for the wrong reason.
func TestDetacher_GrandchildReallyLeavesTheGroup(t *testing.T) {
	c, marker := detachingConn(t)
	waitForFile(t, marker, helperDeadline)
	grandchild := readPID(t, marker)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	if pgid, err := syscall.Getpgid(grandchild); err == nil && pgid == grandchild {
		return // it leads its OWN group, which is the escape being tested
	}
	if pgid, err := syscall.Getpgid(grandchild); err == nil {
		t.Errorf("grandchild %d is in group %d, not detached from the server's", grandchild, pgid)
	} else {
		t.Errorf("grandchild %d could not be inspected: %v", grandchild, err)
	}
	_ = c.Teardown(context.Background())
}
