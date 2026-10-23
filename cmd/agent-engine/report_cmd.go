package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/cursorcli"
)

func cmdRunReport(st *control.Store, id string, dry bool, stdout, stderr io.Writer, asJSON bool) int {
	if dry {
		return fail(stdout, stderr, asJSON, fmt.Errorf("report-only dry run is not execution; use episode validate"))
	}
	rt, err := pinnedRuntime(st.Config(), stderr, false)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	engine, err := control.NewEngine(st, rt, id, control.EngineOptions{})
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	defer engine.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pid := os.Getpid()
	token := control.StartTokenFor(pid)
	if err := st.BeginSupervisor(id, pid, token, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	defer func() {
		kind := control.SupervisorNormalExit
		if ctx.Err() != nil {
			kind = control.SupervisorSignalStop
		}
		if err := st.EndSupervisor(id, pid, token, kind, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			fmt.Fprintln(stderr, err)
		}
	}()
	execute := func(ctx context.Context, p control.ProfileRef, workspace, prompt string, onSpawn func(int) error) (control.ReportTurn, error) {
		if err := recordSpawnIntentFor(st, id, "cursor-report"); err != nil {
			return control.ReportTurn{}, err
		}
		turn, err := cursorcli.Run(ctx, cursorcli.Options{Executable: p.LaunchCommand[0], Model: p.Model, Workspace: workspace, Prompt: prompt, OnSpawned: onSpawn})
		return control.ReportTurn{Text: turn.Text, SessionID: turn.SessionID, Model: turn.ReportedModel}, err
	}
	err = engine.RunReport(ctx, id, execute, func(pid int) error { return recordSpawnedProcessFor(st, id, "cursor-report", pid) })
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	state, err := st.State(id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	return emit(stdout, stderr, map[string]any{"request_id": id, "stage": state.Stage, "status": state.ResultStatus})
}
