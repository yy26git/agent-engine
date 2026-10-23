package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpdates_AtomicQuietRollbackAndRestart(t *testing.T) {
	f, _ := episodeFixture(t)
	id := "forge-run-1842-repair"
	before, err := f.store.State(id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Progress == nil || before.Progress.Latest != 1 {
		t.Fatal("episode has no initial progress", before.Progress)
	}
	_, err = f.store.UpdateCommitted(id, func(s *State) error { s.StageReason = "discarded"; return errors.New("abort") })
	if err == nil {
		t.Fatal("failed transaction passed")
	}
	quiet, err := f.store.UpdateCommitted(id, func(*State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Progress, quiet.Progress) {
		t.Fatal("no-op produced event")
	}
	changed, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.Episode.Phase = string(PhasePlanAccept)
		s.Episode.PlanDigest = strings.Repeat("a", 64)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	last := changed.Progress.Events[len(changed.Progress.Events)-1]
	if last.Kind != "decision-required" || !last.WaitingForOperator || last.StateRevision != changed.Revision || last.PlanDigest == "" {
		t.Fatal(last)
	}
	loaded, err := NewStore(f.cfg).State(id)
	if err != nil || !reflect.DeepEqual(loaded.Progress, changed.Progress) {
		t.Fatal("lost committed notification", err)
	}
	clone := loaded.Clone()
	clone.Progress.Events[0].Kind = "mutated"
	if loaded.Progress.Events[0].Kind == "mutated" {
		t.Fatal("clone shares records")
	}
	_, err = f.store.UpdateCommitted(id, func(s *State) error { s.Progress.Latest = 999; return nil })
	if err != nil {
		t.Fatal(err)
	}
	loaded, _ = f.store.State(id)
	if loaded.Progress.Latest != changed.Progress.Latest {
		t.Fatal("caller changed derived ledger")
	}
}

func TestUpdates_RetentionGapAndFutureCursor(t *testing.T) {
	f, _ := episodeFixture(t)
	id := "forge-run-1842-repair"
	for i := 0; i < MaxProgressUpdates+2; i++ {
		_, err := f.store.UpdateCommitted(id, func(s *State) error { s.StageReason = fmt.Sprintf("change-%d", i); return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := f.store.State(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Progress.Events) != MaxProgressUpdates || s.Progress.Oldest != s.Progress.Events[0].Sequence {
		t.Fatal("unbounded history")
	}
	_, gap, err := s.ProgressReplay(0)
	if err != nil || !gap {
		t.Fatal("missing explicit retention gap", err)
	}
	_, _, err = s.ProgressReplay(s.Progress.Latest + 1)
	if err == nil {
		t.Fatal("future cursor accepted")
	}
	events, gap, err := s.ProgressReplay(s.Progress.Latest - 1)
	if err != nil || gap || len(events) != 1 {
		t.Fatal(events, gap, err)
	}
}

func TestUpdates_MigrationDoesNotInventHistory(t *testing.T) {
	f, _ := episodeFixture(t)
	id := "forge-run-1842-repair"
	s, _ := f.store.State(id)
	s.SchemaVersion = 3
	s.Progress = nil
	b, _ := json.Marshal(s)
	path := filepath.Join(f.root, RequestsDirName, id, StateFileName)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := f.store.State(id)
	if err != nil || loaded.SchemaVersion != CurrentSchemaVersion || loaded.Progress != nil {
		t.Fatal(loaded, err)
	}
	actual, _ := os.ReadFile(path)
	if string(actual) != string(b) {
		t.Fatal("observation persisted migration")
	}
	committed, err := f.store.UpdateCommitted(id, func(*State) error { return nil })
	if err != nil || committed.Progress.Latest != 1 {
		t.Fatal(committed, err)
	}
}

// The production phase engine, not a synthetic PLAN_ACCEPT mutation, produces
// the notification. An observer cannot advance or approve the retained plan.
func TestUpdates_RealPhaseEngineDecisionAndSingleImplementation(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	if _, err := f.store.InitializeDeployment(); err != nil {
		t.Fatal(err)
	}
	admitEpisode(t, f, "EP-PROGRESS")
	initial, _, err := f.store.ProgressFrames("EP-PROGRESS", "")
	if err != nil || len(initial) != 1 {
		t.Fatal(err, initial)
	}
	rt := newFakeRuntime()
	engine, stop := episodeEngine(t, f, rt, "EP-PROGRESS")
	defer stop()
	driveToImplement(t, engine, f, rt, "EP-PROGRESS")
	frames, state, err := f.store.ProgressFrames("EP-PROGRESS", initial[0].EventID)
	if err != nil {
		t.Fatal(err)
	}
	var decisions int
	for _, frame := range frames {
		if frame.WaitingForOperator {
			decisions++
			if frame.PlanDigest == "" || !frame.RequiresAction {
				t.Fatal("decision missing digest/action", frame)
			}
		}
	}
	if decisions != 1 {
		t.Fatalf("decision notifications=%d", decisions)
	}
	if !state.Episode.PlanAuthorized || state.Episode.AuthorizedBy != "operator:local-forge" {
		t.Fatal("fixture did not use real operator gate")
	}
	before := len(rt.sentMessages())
	for i := 0; i < 3; i++ {
		drive(t, engine, "EP-PROGRESS", "standard_fix")
	}
	after := mustState(t, f, "EP-PROGRESS")
	if len(rt.sentMessages()) != before {
		t.Fatal("quiet repeated drive duplicated worker instruction")
	}
	replay, _, err := f.store.ProgressFrames("EP-PROGRESS", frames[len(frames)-1].EventID)
	if err != nil || len(replay) != 0 {
		t.Fatal("quiet episode emitted duplicate progress", replay, err)
	}
	if after.Episode.Phase != string(PhaseImplement) {
		t.Fatal("observation advanced phase")
	}
}

func TestUpdates_FailedDurableWritePublishesNothing(t *testing.T) {
	f, _ := episodeFixture(t)
	id := "forge-run-1842-repair"
	before, err := f.store.State(id)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(f.root, RequestsDirName, id)
	defer os.Chmod(directory, 0700)
	_, err = f.store.UpdateCommitted(id, func(s *State) error {
		s.StageReason = "must not be published"
		return os.Chmod(directory, 0500)
	})
	if err == nil {
		t.Fatal("fixture did not fail the durable write")
	}
	if err = os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.State(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || !reflect.DeepEqual(after.Progress, before.Progress) {
		t.Fatal("failed write published update")
	}
}

func TestUpdates_SlowObserverDoesNotHoldWriterLockOrCancel(t *testing.T) {
	f, _ := episodeFixture(t)
	id := "forge-run-1842-repair"
	if _, err := f.store.InitializeDeployment(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- f.store.ObserveProgress(ctx, id, "", 100*time.Millisecond, false, func(ProgressFrame) error {
			close(entered)
			<-release
			return errors.New("observer disconnected")
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("observer did not start")
	}
	committed := make(chan error, 1)
	go func() {
		_, err := f.store.UpdateCommitted(id, func(s *State) error { s.StageReason = "writer still owns state"; return nil })
		committed <- err
	}()
	select {
	case err := <-committed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("slow observer blocked writer")
	}
	state, err := f.store.State(id)
	if err != nil || state.CancellationRequested {
		t.Fatal("observer affected execution", err)
	}
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnect was ignored")
		}
	case <-ctx.Done():
		t.Fatal("observer did not release")
	}
}
