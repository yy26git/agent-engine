package control

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSupervisorLifecycleOwnerProcessHelper is invoked only by the parent test
// below. It holds the actual OS flock until the parent terminates this process.
func TestSupervisorLifecycleOwnerProcessHelper(t *testing.T) {
	root := os.Getenv("AGENT_ENGINE_TEST_OWNER_STORE")
	if root == "" {
		return
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(cfg)
	eng, err := NewEngine(store, newFakeRuntime(), os.Getenv("AGENT_ENGINE_TEST_OWNER_ID"), EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	fmt.Fprintln(os.Stdout, "OWNER_READY")
	select {}
}

func TestSupervisorLifecycle_ReconcilesIntentionalStopAndAbruptDeath(t *testing.T) {
	f, _ := episodeFixture(t)
	const token = "measured-start-token"
	const pid = 12345
	if err := f.store.BeginSupervisor("forge-run-1842-repair", pid, token, "2026-10-14T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RequestSupervisorStop("forge-run-1842-repair", pid, token, "service shutdown", "2026-10-14T10:01:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EndSupervisor("forge-run-1842-repair", pid, token, SupervisorNormalExit, "2026-10-14T10:01:30Z"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BeginSupervisor("forge-run-1842-repair", pid+1, token+"-2", "2026-10-14T10:02:00Z"); err != nil {
		t.Fatal(err)
	}
	state, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatal(err)
	}
	if state.Supervisor.LastExitKind != SupervisorIntentionalStop || state.Supervisor.State != SupervisorRunning {
		t.Fatalf("service-requested stop was not retained across resume: %+v", state.Supervisor)
	}
	if err := f.store.BeginSupervisor("forge-run-1842-repair", pid+2, token+"-3", "2026-10-14T10:03:00Z"); err != nil {
		t.Fatal(err)
	}
	state, err = f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatal(err)
	}
	if state.Supervisor.LastExitKind != SupervisorAbruptDeath || state.Supervisor.State != SupervisorRunning {
		t.Fatalf("unclosed running record was not classified as process death: %+v", state.Supervisor)
	}
}

func TestSupervisorLifecycle_StopIntentRequiresExactProcessIdentity(t *testing.T) {
	f, _ := episodeFixture(t)
	if err := f.store.BeginSupervisor("forge-run-1842-repair", 12345, "token", "2026-10-14T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	err := f.store.RequestSupervisorStop("forge-run-1842-repair", 12345, "reused-pid", "service shutdown", "2026-10-14T10:01:00Z")
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("stop intent for a reused pid was accepted: %v", err)
	}
}

func TestSupervisorLifecycle_ConcurrentProcessCannotAcquireOrReplaceOwner(t *testing.T) {
	f, _ := episodeFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorLifecycleOwnerProcessHelper$")
	cmd.Env = append(os.Environ(),
		"AGENT_ENGINE_TEST_OWNER_STORE="+f.root,
		"AGENT_ENGINE_TEST_OWNER_ID=forge-run-1842-repair",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childReaped := false
	cleanChild := func() {
		if childReaped {
			return
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		childReaped = true
	}
	defer cleanChild()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if strings.TrimSpace(line) != "OWNER_READY" {
			t.Fatalf("child did not establish request ownership: %q stderr=%s", line, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not establish ownership within 5 seconds: " + stderr.String())
	}
	if _, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{}); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("concurrent process did not observe the existing owner: %v", err)
	}
	cleanChild()
	eng, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatalf("ownership was not available after child exit: %v", err)
	}
	eng.Close()
}
