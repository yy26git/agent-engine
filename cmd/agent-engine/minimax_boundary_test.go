package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
)

func TestMiniMaxBoundary_PinnedLaunchRefusedBeforeSpawnIntent(t *testing.T) {
	storeRoot, workspace := newCLIFixture(t)
	admitRequest(t, storeRoot, workspace, "unqualified-pinned")
	st := storeFor(t, storeRoot)
	_, err := st.UpdateCommitted("unqualified-pinned", func(s *control.State) error {
		s.ExecutionProfiles = map[control.ProfileRole]control.ProfileRef{
			control.RoleWorker: {
				Name: "mini-worker", Role: control.RoleWorker, Transport: control.TransportMiniMaxACP,
				Tool: "mcode", Proven: true, Model: "m:minimax:MiniMax-M2.7:v:thinking",
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	backends, stop, err := admitPinnedTransports(context.Background(), st, "unqualified-pinned", workspace, false)
	defer stop()
	if !errors.Is(err, control.ErrConfinementUnproven) || !strings.Contains(err.Error(), "termination domain") {
		t.Fatalf("pinned unqualified transport was not refused before release/spawn: %v", err)
	}
	if len(backends) != 0 || len(readOwned(t, storeRoot, "unqualified-pinned")) != 0 {
		t.Fatal("refused transport returned a backend or committed a spawn intent")
	}
}

func TestMiniMaxBoundary_LegacyLaunchCannotBypassAdmission(t *testing.T) {
	// A legacy --acp-model route must refuse before accessing a store, launcher,
	// credentials or process. No profile is available on that route to qualify.
	_, err := startACP(context.Background(), nil, "legacy", "/workspace", "M2.7", "bypassPermissions")
	if !errors.Is(err, control.ErrConfinementUnproven) || !strings.Contains(err.Error(), "termination domain") {
		t.Fatalf("legacy launch bypassed the boundary refusal: %v", err)
	}
}

func TestMiniMaxBoundary_CompiledEpisodeAdmissionRefusesBeforeCommit(t *testing.T) {
	storeRoot := episodeFixtureWithTemplate(t)
	path := filepath.Join(storeRoot, "CONTROL.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := "[profiles.investigator]\nrole = \"worker\"\ntool = \"mcode\""
	replacement := old + "\ntransport = \"minimax-acp\"\nregion = \"cn\"\nconfinement_proven = \"true\""
	if strings.Count(string(b), old) != 1 {
		t.Fatal("fixture must have exactly one worker profile")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, replacement, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, workspace := readFixtureRoots(t, storeRoot)
	request := episodeRequestFile(t, workspace, "EP-UNQUALIFIED-CLI", "forge-investigation")
	res := runCLI(t, storeRoot, "episode", "admit", "--request-file", request, "--json")
	if res.code == 0 || !strings.Contains(res.stdout+res.stderr, "termination domain") {
		t.Fatalf("compiled admission did not reach the whole-process gate: %s", res)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "REQUESTS", "EP-UNQUALIFIED-CLI")); !os.IsNotExist(err) {
		t.Fatalf("refusal created a request directory: %v", err)
	}
}
