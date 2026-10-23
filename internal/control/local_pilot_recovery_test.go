package control

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLocalPilotRecovery_AbsentACPHostCanBeCancelledWithoutReplacement(t *testing.T) {
	f, ep := pilotFixture(t, "EP-RECOVERY")
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	token := pidStartToken(pid)
	if token == "" {
		child.Process.Kill()
		child.Wait()
		t.Fatal("no measured process identity")
	}
	child.Process.Kill()
	child.Wait()
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error {
		s.WorkerSession = "retained-native-chat"
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleWorker: {Backend: "minimax-acp", SessionID: s.WorkerSession, ProcessPID: pid, ProcessStart: token, Resolved: true}}
		s.OwnedProcesses = map[string]OwnedProcess{"minimax-acp:impl": {PID: pid, StartToken: token}}
		s.CancellationRequested = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(f.store, newFakeRuntime(), ep.EpisodeID, EngineOptions{Once: true, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if !e.retiredLocalACPSession(ep.EpisodeID, "retained-native-chat") {
		t.Fatal("retired host not recognised")
	}
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error {
		b := s.BackendBindings[RoleWorker]
		b.Backend = "minimax-acp:impl"
		s.BackendBindings[RoleWorker] = b
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !e.retiredLocalACPSession(ep.EpisodeID, "retained-native-chat") {
		t.Fatal("profile-specific retired host not recognised")
	}
	// Partial/corrupt identity, a live process and unresolved spawn all refuse.
	for _, change := range []func(*State){
		func(s *State) { s.OwnedProcesses = nil },
		func(s *State) {
			b := s.BackendBindings[RoleWorker]
			b.Backend = "minimax-acp:other-profile"
			s.BackendBindings[RoleWorker] = b
		},
		func(s *State) {
			r := s.OwnedProcesses["minimax-acp:impl"]
			r.StartToken = "different"
			s.OwnedProcesses["minimax-acp:impl"] = r
		},
		func(s *State) { s.OwnedProcesses["pending"] = OwnedProcess{Pending: true} },
		func(s *State) {
			b := s.BackendBindings[RoleWorker]
			b.ProcessPID = os.Getpid()
			b.ProcessStart = pidStartToken(os.Getpid())
			s.BackendBindings[RoleWorker] = b
			s.OwnedProcesses = map[string]OwnedProcess{"minimax-acp:impl": {PID: b.ProcessPID, StartToken: b.ProcessStart}}
		},
	} {
		original, _ := f.store.State(ep.EpisodeID)
		if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error { change(s); return nil }); err != nil {
			t.Fatal(err)
		}
		if e.retiredLocalACPSession(ep.EpisodeID, "retained-native-chat") {
			t.Fatal("uncertain/live ownership passed recovery")
		}
		if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error {
			s.BackendBindings = original.BackendBindings
			s.OwnedProcesses = original.OwnedProcesses
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A dead leader is insufficient: a native tool can remain in its group.
	original, _ := f.store.State(ep.EpisodeID)
	group := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	group.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := group.Start(); err != nil {
		t.Fatal(err)
	}
	groupPID := group.Process.Pid
	defer syscall.Kill(-groupPID, syscall.SIGKILL)
	groupToken := pidStartToken(groupPID)
	if groupToken == "" {
		group.Process.Kill()
		group.Wait()
		t.Fatal("group identity missing")
	}
	// Wait for the shell's child so this genuinely exercises a surviving group.
	deadline := time.Now().Add(3 * time.Second)
	for {
		output, err := exec.Command("pgrep", "-P", fmt.Sprint(groupPID)).Output()
		if err == nil && len(output) > 0 {
			break
		}
		if time.Now().After(deadline) {
			group.Process.Kill()
			group.Wait()
			t.Fatal("shell never spawned its tool")
		}
		time.Sleep(10 * time.Millisecond)
	}
	group.Process.Kill()
	group.Wait()
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error {
		b := s.BackendBindings[RoleWorker]
		b.ProcessPID = groupPID
		b.ProcessStart = groupToken
		s.BackendBindings[RoleWorker] = b
		s.OwnedProcesses = map[string]OwnedProcess{"minimax-acp:impl": {PID: groupPID, StartToken: groupToken}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if e.retiredLocalACPSession(ep.EpisodeID, "retained-native-chat") {
		t.Fatal("dead leader concealed a live native tool")
	}
	syscall.Kill(-groupPID, syscall.SIGKILL)
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error {
		s.BackendBindings = original.BackendBindings
		s.OwnedProcesses = original.OwnedProcesses
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := e.ReconcileCancellation(context.Background(), ep.EpisodeID)
	if err != nil || !rep.Quiesced || !rep.Committed {
		t.Fatalf("cancel without ACP: %+v, %v", rep, err)
	}
	s, err := f.store.State(ep.EpisodeID)
	if err != nil || s.WorkerSession != "retained-native-chat" || s.Stage != StageCancelled {
		t.Fatal("cancellation lost retained chat or terminal state")
	}
}
