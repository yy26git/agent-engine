package control

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

func releaseEpisodeFixture(t *testing.T, id string) *fixture {
	t.Helper()
	f := execFixture(t, fullTemplate())
	admitEpisode(t, f, id)
	if _, err := f.store.UpdateCommitted(id, func(st *State) error {
		st.Episode.Phase = string(PhaseImplement)
		st.Episode.PlanAuthorized = true
		st.Episode.PlanDigest = strings.Repeat("a", 64)
		st.Episode.PlanBaseline = &Snapshot{ContentDigest: strings.Repeat("b", 64)}
		st.Generation = 2
		st.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "worker-session", Generation: 2, Resolved: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestEpisodeWorkerRelease_RequiresCurrentBindingDigest(t *testing.T) {
	f := releaseEpisodeFixture(t, "EP-RELEASE-DIGEST")
	if _, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-DIGEST", strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted a stale worker binding digest")
	}
	st := mustState(t, f, "EP-RELEASE-DIGEST")
	if st.EpisodeWorkerRelease != nil {
		t.Fatal("stale digest wrote a release intent")
	}
	valid, err := WorkerReleaseBindingDigest(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateCommitted("EP-RELEASE-DIGEST", func(cur *State) error {
		cur.Episode.PlanBaseline.ContentDigest = strings.Repeat("c", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-DIGEST", valid); err == nil {
		t.Fatal("accepted a digest after the pinned source baseline changed")
	}
}

func TestEpisodeWorkerRelease_IsIdempotentAndBlocksOnlyUnsentWorkerEffects(t *testing.T) {
	f := releaseEpisodeFixture(t, "EP-RELEASE-IDEMPOTENT")
	if _, err := f.store.UpdateCommitted("EP-RELEASE-IDEMPOTENT", func(st *State) error {
		st.Effects = []Effect{
			{EffectID: "worker-pending", Role: string(RoleWorker), Status: EffectPending},
			{EffectID: "worker-dispatching", Role: string(RoleWorker), Status: EffectDispatching},
			{EffectID: "reviewer-pending", Role: string(RoleReviewer), Status: EffectPending},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, f, "EP-RELEASE-IDEMPOTENT")
	digest, err := WorkerReleaseBindingDigest(st)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-IDEMPOTENT", digest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-IDEMPOTENT", digest)
	if err != nil {
		t.Fatal(err)
	}
	if first.BindingSHA256 != second.BindingSHA256 || second.Status != WorkerReleaseRequested {
		t.Fatalf("same request was not idempotent: first=%+v second=%+v", first, second)
	}
	got := mustState(t, f, "EP-RELEASE-IDEMPOTENT")
	if got.Effects[0].Status != EffectBlocked || !strings.Contains(got.Effects[0].LastError, "not sent") {
		t.Fatalf("pending worker effect was not honestly blocked: %+v", got.Effects[0])
	}
	if got.Effects[1].Status != EffectDispatching {
		t.Fatalf("uncertain in-flight worker effect was rewritten: %+v", got.Effects[1])
	}
	if got.Effects[2].Status != EffectPending {
		t.Fatalf("unrelated reviewer effect was changed: %+v", got.Effects[2])
	}
	if got.ExecutionProfiles[RoleWorker].Name != first.Profile || got.BackendBindings[RoleWorker].SessionID != first.SessionID {
		t.Fatal("release intent mutated the pinned profile or worker binding")
	}
	if _, err := f.store.UpdateCommitted("EP-RELEASE-IDEMPOTENT", func(st *State) error {
		st.Effects = append(st.Effects, Effect{EffectID: "racing-worker-effect", Role: string(RoleWorker), Status: EffectPending})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(f.store, newFakeRuntime())
	if _, err := dispatcher.claim("EP-RELEASE-IDEMPOTENT", "racing-worker-effect"); err != errEffectGone {
		t.Fatalf("worker effect crossed a durable release intent: %v", err)
	}
	out, err := f.store.DriveEpisode("EP-RELEASE-IDEMPOTENT", "standard_fix")
	if err != nil {
		t.Fatal(err)
	}
	if out.Advanced != "" || !strings.Contains(out.Blocked, "worker release") {
		t.Fatalf("episode advanced despite release intent: %+v", out)
	}
}

func TestEpisodeWorkerRelease_SharedBackendBlocksWithoutShutdown(t *testing.T) {
	f := releaseEpisodeFixture(t, "EP-RELEASE-SHARED")
	if _, err := f.store.UpdateCommitted("EP-RELEASE-SHARED", func(st *State) error {
		st.BackendBindings[RoleReviewer] = SessionBinding{Backend: "minimax-acp", SessionID: "reviewer-session", Resolved: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, f, "EP-RELEASE-SHARED")
	digest, err := WorkerReleaseBindingDigest(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-SHARED", digest); err != nil {
		t.Fatal(err)
	}
	backend := &episodeTerminationBackend{terminationBackend: newTerminationBackend()}
	eng, err := NewEngine(f.store, newFakeRuntime(), "EP-RELEASE-SHARED", EngineOptions{
		Now: func() time.Time { return f.now }, Backends: []runtime.Backend{backend},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	out, err := eng.DriveEpisode(context.Background(), "EP-RELEASE-SHARED", "standard_fix")
	if err != nil {
		t.Fatal(err)
	}
	got := mustState(t, f, "EP-RELEASE-SHARED").EpisodeWorkerRelease
	if got.Status != WorkerReleaseBlocked || !strings.Contains(got.Reason, "shared process release is refused") {
		t.Fatalf("shared backend was not blocked: %+v (%+v)", got, out)
	}
	if len(backend.shutdown) != 0 || backend.stops != 0 {
		t.Fatalf("shared transport was touched: shutdown=%v process stops=%d", backend.shutdown, backend.stops)
	}
}

func TestEpisodeWorkerRelease_RecoversReceiptAfterOwnedProcessWasReaped(t *testing.T) {
	f := releaseEpisodeFixture(t, "EP-RELEASE-RECOVERY")
	const missingPID = 2147483000
	if _, err := f.store.UpdateCommitted("EP-RELEASE-RECOVERY", func(st *State) error {
		st.BackendBindings[RoleWorker] = SessionBinding{Backend: "minimax-acp", SessionID: "old-worker", Generation: 2,
			ProcessPID: missingPID, ProcessStart: "old-start-token", Resolved: true}
		st.OwnedProcesses = map[string]OwnedProcess{"minimax-acp": {
			PID: missingPID, StartToken: "old-start-token", RecordedAt: f.now.UTC().Format(time.RFC3339Nano),
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, f, "EP-RELEASE-RECOVERY")
	digest, err := WorkerReleaseBindingDigest(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-RECOVERY", digest); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(f.store, newFakeRuntime(), "EP-RELEASE-RECOVERY", EngineOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	if problems := eng.ReapOrphanedProcesses(context.Background(), "EP-RELEASE-RECOVERY"); len(problems) != 0 {
		t.Fatalf("old worker identity was not reaped before receipt recovery: %v", problems)
	}
	if err := eng.recoverEpisodeWorkerReleaseAfterReap(context.Background(), "EP-RELEASE-RECOVERY"); err != nil {
		t.Fatal(err)
	}
	recovered := mustState(t, f, "EP-RELEASE-RECOVERY").EpisodeWorkerRelease
	if recovered.Status != WorkerReleaseReleased || !strings.Contains(recovered.QuiescenceProof, "process group is empty") {
		t.Fatalf("reaped worker was not recovered with an explicit proof: %+v", recovered)
	}
}

type releaseOwnedProcessBackend struct {
	*episodeTerminationBackend
	cmd *exec.Cmd
}

func (b *releaseOwnedProcessBackend) StopOwnedProcesses(ctx context.Context) error {
	b.stops++
	if err := syscall.Kill(-b.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- b.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return err
			}
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := syscall.Kill(-b.pid, 0); !errors.Is(err, syscall.ESRCH) {
		return errors.New("test process group survived its leader")
	}
	b.pid = 0
	return nil
}

func TestEpisodeWorkerRelease_StopsExclusiveRecordedProcessAndPersistsReceipt(t *testing.T) {
	f := releaseEpisodeFixture(t, "EP-RELEASE-PROCESS")
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	start := pidStartToken(cmd.Process.Pid)
	if start == "" {
		t.Fatal("test worker process has no measurable start token")
	}
	if _, err := f.store.UpdateCommitted("EP-RELEASE-PROCESS", func(st *State) error {
		st.BackendBindings[RoleWorker] = SessionBinding{Backend: "minimax-acp", SessionID: "worker-process-session", Generation: 2,
			ProcessPID: cmd.Process.Pid, ProcessStart: start, Resolved: true}
		st.OwnedProcesses = map[string]OwnedProcess{"minimax-acp": {
			PID: cmd.Process.Pid, StartToken: start, RecordedAt: f.now.UTC().Format(time.RFC3339Nano),
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, f, "EP-RELEASE-PROCESS")
	digest, err := WorkerReleaseBindingDigest(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-PROCESS", digest); err != nil {
		t.Fatal(err)
	}
	base := &episodeTerminationBackend{terminationBackend: newTerminationBackend()}
	base.pid = cmd.Process.Pid
	base.liveness["worker-process-session"] = "active"
	backend := &releaseOwnedProcessBackend{episodeTerminationBackend: base, cmd: cmd}
	eng, err := NewEngine(f.store, newFakeRuntime(), "EP-RELEASE-PROCESS", EngineOptions{
		Now: func() time.Time { return f.now }, Backends: []runtime.Backend{backend},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	out, err := eng.DriveEpisode(context.Background(), "EP-RELEASE-PROCESS", "standard_fix")
	if err != nil {
		t.Fatal(err)
	}
	got := mustState(t, f, "EP-RELEASE-PROCESS")
	if got.EpisodeWorkerRelease.Status != WorkerReleaseReleased || !strings.Contains(got.EpisodeWorkerRelease.QuiescenceProof, "process ") {
		t.Fatalf("exclusive process release has no quiescence receipt: %+v (%+v)", got.EpisodeWorkerRelease, out)
	}
	if backend.stops != 1 || len(backend.shutdown) != 1 || backend.shutdown[0] != "worker-process-session" {
		t.Fatalf("release did not target exactly the worker: process stops=%d sessions=%v", backend.stops, backend.shutdown)
	}
	if alive, known := recordedProcessAlive(cmd.Process.Pid, start); !known || alive {
		t.Fatalf("released worker identity is not proven gone: alive=%v known=%v", alive, known)
	}
	if got.ExecutionProfiles[RoleWorker].Name != got.EpisodeWorkerRelease.Profile || got.BackendBindings[RoleWorker].SessionID != "worker-process-session" {
		t.Fatal("release changed the pinned worker profile or historical binding")
	}
}

func TestEpisodeWorkerRelease_ShutsDownProcesslessAgentDeckSession(t *testing.T) {
	f := releaseEpisodeFixture(t, "EP-RELEASE-AGENTDECK")
	if _, err := f.store.UpdateCommitted("EP-RELEASE-AGENTDECK", func(st *State) error {
		st.BackendBindings[RoleWorker] = SessionBinding{Backend: "agent-deck", SessionID: "agentdeck-worker", Generation: 2, Resolved: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, f, "EP-RELEASE-AGENTDECK")
	digest, err := WorkerReleaseBindingDigest(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RequestEpisodeWorkerRelease("EP-RELEASE-AGENTDECK", digest); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	rt.createdIDs = append(rt.createdIDs, "agentdeck-worker")
	eng, err := NewEngine(f.store, rt, "EP-RELEASE-AGENTDECK", EngineOptions{
		Once: true, Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	run, err := eng.Run(context.Background(), "EP-RELEASE-AGENTDECK")
	if err != nil {
		t.Fatal(err)
	}
	got := mustState(t, f, "EP-RELEASE-AGENTDECK").EpisodeWorkerRelease
	if got.Status != WorkerReleaseReleased || got.QuiescenceProof != "bound session reported closed" {
		t.Fatalf("active supervisor did not verify the processless Agent Deck session closed: %+v (%+v)", got, run)
	}
	if len(rt.stopped) != 1 || rt.stopped[0] != "agentdeck-worker" {
		t.Fatalf("release stopped wrong Agent Deck sessions: %v", rt.stopped)
	}
}
