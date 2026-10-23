package control

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/emjin/agent-engine/internal/runtime"
)

func TestTransportOwnership_ActivationPersistsMeasuredProcessIdentity(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	profile := Profile{Name: "acp-worker", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode"}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: profile.Ref()}
		grantActivationFixture(t, s, f.srcRoot, profile)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{name: "minimax-acp", pid: os.Getpid()}
	e, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{Backends: []runtime.Backend{backend}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.LaunchProfile(context.Background(), "forge-run-1842-repair", RoleWorker, profile.Name, f.srcRoot, "identity"); err != nil {
		t.Fatal(err)
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatal(err)
	}
	binding := st.BackendBindings[RoleWorker]
	if binding.ProcessPID != os.Getpid() || binding.ProcessStart == "" || binding.ProcessStart != pidStartToken(os.Getpid()) {
		t.Fatalf("confirmed activation lost measured process identity: %+v", binding)
	}
}

func TestTransportOwnership_ReapsBeforeFactoryAndRetainsOwnershipThroughStop(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid, cleanup := startSleeper(t)
	defer cleanup()
	token := pidStartToken(pid)
	if token == "" {
		t.Fatal("real process identity was not measured")
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "old", ProcessPID: pid, ProcessStart: token, Resolved: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	assertOwned := func() {
		t.Helper()
		release, err := f.store.AcquireOwnership("forge-run-1842-repair")
		if release != nil {
			release()
		}
		if !errors.Is(err, ErrOwnershipHeld) {
			t.Fatalf("transport lifecycle lost ownership: %v", err)
		}
	}
	started, stopped := false, false
	err = e.InitializeTransports(context.Background(), "forge-run-1842-repair", func() ([]runtime.Backend, func(), error) {
		assertOwned()
		if alive, known := recordedProcessAlive(pid, token); !known || alive {
			t.Fatalf("factory reached before orphan was proven gone: alive=%v known=%v", alive, known)
		}
		started = true
		return nil, func() { assertOwned(); stopped = true }, nil
	})
	if err != nil || !started {
		t.Fatalf("initialize: started=%v err=%v", started, err)
	}
	e.Close()
	if !stopped {
		t.Fatal("transport was not stopped before release")
	}
	release, err := f.store.AcquireOwnership("forge-run-1842-repair")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestTransportOwnership_UnknownIdentityPreventsFactory(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid, cleanup := startSleeper(t)
	defer cleanup()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", ProcessPID: pid},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	called := false
	err = e.InitializeTransports(context.Background(), "forge-run-1842-repair", func() ([]runtime.Backend, func(), error) {
		called = true
		return nil, nil, nil
	})
	if err == nil || called {
		t.Fatalf("unidentified live process allowed startup: called=%v err=%v", called, err)
	}
}
