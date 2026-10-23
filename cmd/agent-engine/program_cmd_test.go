package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func TestProgramCLI_AdmitsOnlyLocallyPinnedSingleRootCard(t *testing.T) {
	store, source := flowCLIFixture(t)
	requestBody := map[string]any{
		"version": 1, "episode_id": "program-cli-child", "template": "feature",
		"origin":    map[string]string{"kind": "agent", "caller_id": "codex-local"},
		"workspace": map[string]string{"path": source}, "objective": "Fix addition",
		"obligations":  []map[string]string{{"id": "O-1", "criterion": "Addition regression passes"}},
		"constraints":  map[string]any{"allow_code_changes": true, "allowed_paths": []string{"src/**"}, "protected_paths": []string{"tests/**"}},
		"verification": []map[string]string{{"check_id": "build"}},
	}
	requestPath := writeRequest(t, t.TempDir(), requestBody)
	requestBytes, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	episode, err := contract.DecodeEpisodeRequest(requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	seed := *episode
	seed.EpisodeID = "program-cli-seed"
	seedBytes, _ := json.Marshal(seed)
	seedPath := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seedPath, seedBytes, 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if code := run([]string{"--store", store, "episode", "admit", "--request-file", seedPath}, &out, &diagnostics); code != 0 {
		t.Fatalf("seed admission failed: %s", diagnostics.String())
	}

	cfg, err := control.LoadConfig(store)
	if err != nil {
		t.Fatal(err)
	}
	st := control.NewStore(cfg)
	seedState, err := st.State(seed.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	template, ok := cfg.EpisodeTemplates.Lookup(episode.Template)
	if !ok {
		t.Fatal("fixture template missing")
	}
	templateBytes, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	templateSum := sha256.Sum256(templateBytes)
	snapshot, err := control.TakeSnapshot(source, time.Now(), store)
	if err != nil {
		t.Fatal(err)
	}
	dependencySum := sha256.Sum256([]byte("null")) // empty dependency-file list
	roles := make([]contract.ProgramRoleBinding, 0, len(seedState.ExecutionProfiles))
	roleNames := make([]string, 0, len(seedState.ExecutionProfiles))
	for role := range seedState.ExecutionProfiles {
		roleNames = append(roleNames, string(role))
	}
	sort.Strings(roleNames)
	for _, role := range roleNames {
		ref := seedState.ExecutionProfiles[control.ProfileRole(role)]
		roles = append(roles, contract.ProgramRoleBinding{Role: role, Profile: ref.Name, Model: ref.Model})
	}
	budget := contract.ProgramBudget{Children: 1, RepairCycles: 1, WorkerTurns: 1, AuditAttempts: 1}
	program := &contract.ProgramDefinition{
		Schema: contract.ProgramVersion, ProgramID: "program-cli-test", Revision: 1,
		AuthorityRef: "cli-test", IssuedAt: "2026-10-14T00:00:00Z", Budget: budget,
		Cards: []contract.ProgramCard{{
			CardID: "card-1", DependsOn: []string{}, Objective: episode.Objective, Template: episode.Template,
			TemplateDigest: hex.EncodeToString(templateSum[:]), SourceBaseline: snapshot.ContentDigest,
			DependencyBaseline: hex.EncodeToString(dependencySum[:]), Grant: "read-write", Roles: roles,
			Capabilities:   []string{"managed-checks", "workspace-read", "workspace-write"},
			AllowedPaths:   append([]string{}, episode.Constraints.AllowedPaths...),
			ProtectedPaths: append([]string{}, episode.Constraints.ProtectedPaths...),
			RequiredChecks: []string{"build"}, AllowedChangeKinds: []string{"source"},
			Obligations:  append([]contract.WorkflowObligation{}, episode.Obligations...),
			RepairRoutes: []contract.RepairRoute{}, Resources: []contract.ResourceClaim{}, Budget: budget,
		}},
		Escalations: []contract.Escalation{{ID: "scope-change", When: "a plan differs", Description: "request local operator review"}},
	}
	digest, err := program.ProgramDigest()
	if err != nil {
		t.Fatalf("program digest: %v", err)
	}
	definitionBytes, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(t.TempDir(), "program.json")
	if err := os.WriteFile(definitionPath, definitionBytes, 0600); err != nil {
		t.Fatal(err)
	}
	configFile, err := os.OpenFile(filepath.Join(store, "CONTROL.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(configFile, "\n[program_authorities.cli-test]\nprogram_id = %q\ndefinition_sha256 = %q\nexpires_at = %q\n", program.ProgramID, digest, "2030-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}

	call := func() int {
		out.Reset()
		diagnostics.Reset()
		return run([]string{"--store", store, "--json", "program", "admit-card", "--definition-file", definitionPath, "--card-id", "card-1", "--request-file", requestPath}, &out, &diagnostics)
	}
	if code := call(); code != 0 {
		t.Fatalf("program card admission failed: %s", diagnostics.String())
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["episode_id"] != episode.EpisodeID || result["program_id"] != program.ProgramID || result["card_id"] != "card-1" || result["phase"] != string(control.PhaseReport) {
		t.Fatalf("unexpected admission receipt: %s", out.String())
	}
	cfg, err = control.LoadConfig(store)
	if err != nil {
		t.Fatal(err)
	}
	state, err := control.NewStore(cfg).State(episode.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ProgramBinding == nil || state.ProgramBinding.ProgramDigest != digest || state.ProgramBinding.CardID != "card-1" || state.Episode.PlanAuthorized || len(state.ProgramAcceptances) != 0 {
		t.Fatalf("admission lost the pinned card or created authority early: %+v", state.ProgramBinding)
	}
	if code := call(); code != 0 {
		t.Fatalf("exact duplicate admission was not idempotent: %s", diagnostics.String())
	}

	// This command is intentionally not a scheduler. Even an otherwise valid
	// two-card definition cannot bypass parent dependency/budget coordination.
	multi := *program
	multi.Budget.Children = 2
	multi.Cards = append(append([]contract.ProgramCard(nil), program.Cards...), program.Cards[0])
	multi.Cards[1].CardID = "card-2"
	multiBytes, err := json.Marshal(&multi)
	if err != nil {
		t.Fatal(err)
	}
	multiPath := filepath.Join(t.TempDir(), "multi-program.json")
	if err := os.WriteFile(multiPath, multiBytes, 0600); err != nil {
		t.Fatal(err)
	}
	multiRequest := *episode
	multiRequest.EpisodeID = "program-cli-multi"
	multiRequestBytes, _ := json.Marshal(multiRequest)
	multiRequestPath := filepath.Join(t.TempDir(), "multi-episode.json")
	if err := os.WriteFile(multiRequestPath, multiRequestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run([]string{"--store", store, "program", "admit-card", "--definition-file", multiPath, "--card-id", "card-1", "--request-file", multiRequestPath}, &out, &diagnostics); code == 0 {
		t.Fatal("single-card CLI accepted a multi-card program without its coordinator")
	}
	if _, err := control.NewStore(cfg).State(multiRequest.EpisodeID); err == nil {
		t.Fatal("multi-card refusal left a child episode admitted")
	}

	// A caller-edited definition cannot replace the definition pinned in local
	// CONTROL.toml, even when the modified JSON remains structurally valid.
	program.Revision++
	changedBytes, _ := json.Marshal(program)
	if err := os.WriteFile(definitionPath, changedBytes, 0600); err != nil {
		t.Fatal(err)
	}
	changedRequest := *episode
	changedRequest.EpisodeID = "program-cli-changed"
	changedRequestBytes, _ := json.Marshal(changedRequest)
	changedRequestPath := filepath.Join(t.TempDir(), "changed-episode.json")
	if err := os.WriteFile(changedRequestPath, changedRequestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run([]string{"--store", store, "program", "admit-card", "--definition-file", definitionPath, "--card-id", "card-1", "--request-file", changedRequestPath}, &out, &diagnostics); code == 0 {
		t.Fatal("caller-edited program definition bypassed the local digest grant")
	}
	if _, err := control.NewStore(cfg).State(changedRequest.EpisodeID); err == nil {
		t.Fatal("refused changed program left an admitted episode")
	}
}
