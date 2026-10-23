package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/emjin/agent-engine/internal/runtime"
)

func TestEngine_WorkspaceLeaseBlocksAnotherRequestUntilClose(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	first, err := NewEngine(f.store, &fakeRuntime{}, "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	req := f.validRequest()
	req.RequestID = "other-caller"
	if _, err := f.store.Admit(req); err != nil {
		t.Fatal(err)
	}
	if second, err := NewEngine(f.store, &fakeRuntime{}, req.RequestID, EngineOptions{}); !errors.Is(err, ErrWorkspaceBusy) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second production engine did not refuse the shared workspace: %v", err)
	}
	// A cleanup owner can still stop its own effects without acquiring write
	// execution authority from the other request.
	if _, err := f.store.UpdateCommitted(req.RequestID, func(s *State) error { s.CancellationRequested = true; return nil }); err != nil {
		t.Fatal(err)
	}
	cleanup, err := NewEngine(f.store, &fakeRuntime{}, req.RequestID, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup.InitializeTransports(context.Background(), req.RequestID, func() ([]runtime.Backend, func(), error) {
		t.Fatal("cancellation launched a transport")
		return nil, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cleanup.Close()
	if _, err := f.store.UpdateCommitted(req.RequestID, func(s *State) error { s.CancellationRequested = false; return nil }); err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := NewEngine(f.store, &fakeRuntime{}, req.RequestID, EngineOptions{})
	if err != nil {
		t.Fatalf("released workspace was not reusable: %v", err)
	}
	second.Close()
}

func TestEngine_BundleWorkspaceLeaseRollbackDoesNotLeakOwnership(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	target := filepath.Join(f.srcRoot, "z-target")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.RepairBundleTargets = []RepairBundleTargetReceipt{{Workspace: target}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	occupied, err := f.store.AcquireWorkspaceLease(target, "other-caller")
	if err != nil {
		t.Fatal(err)
	}
	held := true
	t.Cleanup(func() {
		if held {
			occupied()
		}
	})
	if eng, err := NewEngine(f.store, &fakeRuntime{}, "forge-run-1842-repair", EngineOptions{}); !errors.Is(err, ErrWorkspaceBusy) {
		if eng != nil {
			eng.Close()
		}
		t.Fatalf("bundle target overlap was accepted: %v", err)
	}
	primary, err := f.store.AcquireWorkspaceLease(filepath.Join(f.srcRoot, "project"), "probe")
	if err != nil {
		t.Fatalf("partial acquisition leaked the primary lease: %v", err)
	}
	primary()
	occupied()
	held = false
	eng, err := NewEngine(f.store, &fakeRuntime{}, "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if release, err := f.store.AcquireWorkspaceLease(target, "probe"); !errors.Is(err, ErrWorkspaceBusy) {
		if release != nil {
			release()
		}
		t.Fatalf("engine did not retain the bundle target lease: %v", err)
	}
}
