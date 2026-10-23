package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

func reportFixture(t *testing.T) (*fixture, *contract.EpisodeRequest) {
	t.Helper()
	f := newFixture(t)
	f.now = time.Now().UTC()
	f.store.Now = func() time.Time { return f.now }
	workspace := filepath.Join(f.srcRoot, "project")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(launcher, []byte("fixture binary"), 0700); err != nil {
		t.Fatal(err)
	}
	digest, err := DigestFile(launcher)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Profiles = map[string]Profile{"reporter": {Name: "reporter", Role: RoleInvestigat, Transport: TransportCursorCLI, Tool: "cursor", Model: "composer-2.5", ReadOnly: true, LaunchCommand: []string{launcher}, LauncherSHA256: digest}}
	tpl := EpisodeTemplate{Name: "report", ReportOnly: true, ReadOnly: true, Phases: []contract.EpisodePhase{contract.PhaseReport}, Roles: map[ProfileRole]string{RoleInvestigat: "reporter"}, MaxRepairIterations: 1}
	if err := f.cfg.EpisodeTemplates.Register(tpl); err != nil {
		t.Fatal(err)
	}
	req := &contract.EpisodeRequest{Version: 1, EpisodeID: "report-1", Template: "report", Origin: contract.Origin{Kind: "forge", CallerID: "forge-report"}, Workspace: contract.Workspace{Path: workspace}, Objective: "Explain supplied evidence", Obligations: []contract.WorkflowObligation{{ID: "report", Criterion: "Report cites evidence"}}, StructuredTask: &contract.StructuredTask{Function: "report-analysis", SchemaName: "fixture", SchemaJSON: `{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}},"additionalProperties":false}`, User: "expected behavior and logs"}}
	return f, req
}
func TestReportOnlyProductionComposition(t *testing.T) {
	f, req := reportFixture(t)
	if _, err := f.store.AdmitEpisode(req); err != nil {
		t.Fatal(err)
	}
	state, err := f.store.State(req.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ExecutionProfiles) != 1 || !state.Episode.ReportOnly {
		t.Fatal("not a single reporting role")
	}
	runtime := newFakeRuntime()
	e, cleanup := episodeEngine(t, f, runtime, req.EpisodeID)
	defer cleanup()
	calls := 0
	run := func(ctx context.Context, p ProfileRef, workspace, prompt string, spawn func(int) error) (ReportTurn, error) {
		calls++
		if workspace == req.Workspace.Path || !strings.Contains(prompt, req.StructuredTask.User) {
			t.Fatal("missing data isolation")
		}
		if p.Model != "composer-2.5" {
			t.Fatal("model changed")
		}
		return ReportTurn{Text: `{"ok":false}`, SessionID: "cursor-chat"}, nil
	}
	if err := e.RunReport(context.Background(), req.EpisodeID, run, nil); err != nil {
		t.Fatal(err)
	}
	output, err := f.store.ReportOutput(req.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := e.ReadEpisodeArtifact(req.EpisodeID, output.ReportSHA256)
	if err != nil || string(retained) != output.OutputJSON {
		t.Fatal("report digest has no exact retained artifact", err)
	}
	state, _ = f.store.State(req.EpisodeID)
	result, err := f.store.Result(req.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != StageReportComplete || result.Status != contract.StatusComplete || !result.Ready || output.SelectedModel != "composer-2.5" || output.ObservedModel != "" || state.ActiveReview != nil || state.Episode.PlanDigest != "" {
		t.Fatalf("false audit/identity: %+v", state)
	}
	if err := e.RunReport(context.Background(), req.EpisodeID, run, nil); err != nil || calls != 1 {
		t.Fatal("duplicate dispatch", err, calls)
	}
}
func TestReportOnlyRefusesInvalidAndInterruptedTurns(t *testing.T) {
	for _, kind := range []string{"schema", "process", "interrupted", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f, req := reportFixture(t)
			if _, err := f.store.AdmitEpisode(req); err != nil {
				t.Fatal(err)
			}
			if kind == "interrupted" || kind == "cancel" {
				_, err := f.store.UpdateCommitted(req.EpisodeID, func(s *State) error {
					s.Episode.ReportTaskStarted = kind == "interrupted"
					s.CancellationRequested = kind == "cancel"
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			e, cleanup := episodeEngine(t, f, newFakeRuntime(), req.EpisodeID)
			defer cleanup()
			calls := 0
			err := e.RunReport(context.Background(), req.EpisodeID, func(context.Context, ProfileRef, string, string, func(int) error) (ReportTurn, error) {
				calls++
				if kind == "process" {
					return ReportTurn{}, fmt.Errorf("fixture failed")
				}
				return ReportTurn{Text: `{"ok":"wrong"}`, SessionID: "chat"}, nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := f.store.State(req.EpisodeID)
			if state.Stage != StageBlocked && state.Stage != StageCancelled {
				t.Fatal(state.Stage)
			}
			if (kind == "interrupted" || kind == "cancel") && calls != 0 {
				t.Fatal("replayed interrupted/cancelled turn")
			}
			if _, err := f.store.ReportOutput(req.EpisodeID); err == nil {
				t.Fatal("failed report published")
			}
		})
	}
}
func TestReportOnlyAdmissionRejectsWritesAndRebinding(t *testing.T) {
	f, req := reportFixture(t)
	req.Constraints.AllowCodeChanges = true
	if _, err := f.store.AdmitEpisode(req); err == nil {
		t.Fatal("write grant accepted")
	}
	req.Constraints.AllowCodeChanges = false
	if _, err := f.store.AdmitEpisode(req); err != nil {
		t.Fatal(err)
	}
	req.StructuredTask.User = "different evidence"
	if _, err := f.store.AdmitEpisode(req); err == nil {
		t.Fatal("changed input under same identity")
	}
	p := f.cfg.Profiles["reporter"]
	p.Proven = true
	if ValidateReportProfile(p) == nil {
		t.Fatal("native ask mode claimed OS confinement")
	}
	p.Proven = false
	p.LauncherSHA256 = strings.Repeat("0", 64)
	if ValidateReportProfile(p) == nil {
		t.Fatal("changed launcher accepted")
	}
	tpl, _ := f.cfg.EpisodeTemplates.Lookup("report")
	tpl.Phases = append(tpl.Phases, contract.PhasePlan)
	if tpl.validateAt("test") == nil {
		t.Fatal("report-only planning accepted")
	}
}

func TestReportOnlyCancellationWinsPublicationAndResume(t *testing.T) {
	f, req := reportFixture(t)
	if _, err := f.store.AdmitEpisode(req); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.UpdateCommitted(req.EpisodeID, func(s *State) error {
		s.Episode.ReportOutput = &StructuredReportOutput{OutputJSON: `{"ok":true}`}
		s.CancellationRequested = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e, cleanup := episodeEngine(t, f, newFakeRuntime(), req.EpisodeID)
	defer cleanup()
	if err := e.RunReport(context.Background(), req.EpisodeID, func(context.Context, ProfileRef, string, string, func(int) error) (ReportTurn, error) {
		t.Fatal("cancelled report executed")
		return ReportTurn{}, nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	state, _ := f.store.State(req.EpisodeID)
	if state.Stage != StageCancelled {
		t.Fatal(state.Stage)
	}
}

func TestReportOnlyInflightCancellationStopsBeforeOutput(t *testing.T) {
	f, req := reportFixture(t)
	if _, err := f.store.AdmitEpisode(req); err != nil {
		t.Fatal(err)
	}
	e, cleanup := episodeEngine(t, f, newFakeRuntime(), req.EpisodeID)
	defer cleanup()
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	go func() {
		<-started
		_, err := f.store.UpdateCommitted(req.EpisodeID, func(s *State) error { s.CancellationRequested = true; return nil })
		cancelled <- err
	}()
	err := e.RunReport(context.Background(), req.EpisodeID, func(ctx context.Context, _ ProfileRef, _, _ string, _ func(int) error) (ReportTurn, error) {
		close(started)
		select {
		case <-ctx.Done():
			return ReportTurn{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return ReportTurn{}, fmt.Errorf("durable cancellation was not observed")
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	state, _ := f.store.State(req.EpisodeID)
	if state.Stage != StageCancelled || state.Episode.ReportOutput != nil {
		t.Fatal("cancelled turn published output", state.Stage)
	}
}
