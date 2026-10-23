package main

import (
	"os"
	"testing"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func TestDiscoverySnapshotExposesSupervisorExitClassification(t *testing.T) {
	root := episodeFixtureWithTemplate(t)
	_, source := readFixtureRoots(t, root)
	const episodeID = "forge-SUPERVISOR-STATUS"
	requestPath := episodeRequestFile(t, source, episodeID, "forge-investigation")
	requestBytes, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	episodeRequest, err := contract.DecodeEpisodeRequest(requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	store := storeFor(t, root)
	if _, err := store.AdmitEpisode(episodeRequest); err != nil {
		t.Fatalf("admit episode: %v", err)
	}
	if _, err := store.UpdateCommitted(episodeID, func(s *control.State) error {
		s.Supervisor = &control.SupervisorLifecycle{PID: 1234, StartToken: "measured", StartedAt: "2026-10-14T00:00:00Z", State: control.SupervisorRunning}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	api := &episodeAPI{store: store, callerPrefix: "forge-", templates: map[string]bool{"forge-investigation": true}}
	entries, err := api.episodeSnapshot(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SupervisorExit != "unclassified_exit" {
		t.Fatalf("snapshot omitted the stale supervisor distinction: %+v", entries)
	}
}
