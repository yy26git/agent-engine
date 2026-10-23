package control

import (
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

func TestLoadProgramAuthoritiesPinsDefinitionAndExpiry(t *testing.T) {
	definition := localProgramDefinition()
	digest, err := definition.ProgramDigest()
	if err != nil {
		t.Fatal(err)
	}
	loaded := &Config{}
	doc := map[string]any{"program_authorities": map[string]any{
		"grant-1": map[string]any{"program_id": "sample-program", "definition_sha256": digest, "expires_at": "2026-10-15T00:00:00Z"},
	}}
	if err := loadProgramAuthorities(doc, loaded); err != nil {
		t.Fatalf("load local program grant: %v", err)
	}
	if got := loaded.ProgramAuthorities["grant-1"]; got.ProgramID != definition.ProgramID || got.DefinitionSHA256 != digest {
		t.Fatalf("local program pin was not retained: %+v", got)
	}

	for name, bad := range map[string]map[string]any{
		"uppercase digest": {"program_id": definition.ProgramID, "definition_sha256": strings.ToUpper(digest), "expires_at": "2026-10-15T00:00:00Z"},
		"malformed expiry": {"program_id": definition.ProgramID, "definition_sha256": digest, "expires_at": "tomorrow"},
		"unknown setting":  {"program_id": definition.ProgramID, "definition_sha256": digest, "expires_at": "2026-10-15T00:00:00Z", "accept_all": true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := loadProgramAuthorities(map[string]any{"program_authorities": map[string]any{"grant-1": bad}}, &Config{}); err == nil {
				t.Fatal("invalid local grant was accepted")
			}
		})
	}
}

func TestVerifyProgramAuthorityRequiresExactLocallyPinnedDefinition(t *testing.T) {
	program := localProgramDefinition()
	digest, err := program.ProgramDigest()
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(&Config{ProgramAuthorities: map[string]ProgramAuthorityGrant{
		program.AuthorityRef: {AuthorityRef: program.AuthorityRef, ProgramID: program.ProgramID, DefinitionSHA256: digest, ExpiresAt: "2026-10-15T00:00:00Z"},
	}})
	store.Now = func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }
	if got, err := store.VerifyProgramAuthority(program); err != nil || got != digest {
		t.Fatalf("exact locally pinned definition refused: digest=%s err=%v", got, err)
	}

	changed := *program
	changed.Cards = append([]contract.ProgramCard(nil), program.Cards...)
	changed.Cards[0].Objective = "changed objective"
	if _, err := store.VerifyProgramAuthority(&changed); err == nil {
		t.Fatal("caller-changed program definition matched a pinned local digest")
	}

	expired := *store.cfg
	expired.ProgramAuthorities = map[string]ProgramAuthorityGrant{
		program.AuthorityRef: {AuthorityRef: program.AuthorityRef, ProgramID: program.ProgramID, DefinitionSHA256: digest, ExpiresAt: "2026-10-14T07:59:59Z"},
	}
	expiredStore := NewStore(&expired)
	expiredStore.Now = store.Now
	if _, err := expiredStore.VerifyProgramAuthority(program); err == nil {
		t.Fatal("expired local program grant was accepted")
	}
}

func localProgramDefinition() *contract.ProgramDefinition {
	sha := strings.Repeat("a", 64)
	return &contract.ProgramDefinition{
		Schema: contract.ProgramVersion, ProgramID: "sample-program", Revision: 1,
		AuthorityRef: "grant-1", IssuedAt: "2026-10-14T00:00:00Z",
		Budget: contract.ProgramBudget{Children: 1, RepairCycles: 1, WorkerTurns: 2, AuditAttempts: 1},
		Cards: []contract.ProgramCard{{
			CardID: "card-1", DependsOn: []string{}, Objective: "Make the sample check pass", Template: "repair",
			TemplateDigest: sha, SourceBaseline: sha, DependencyBaseline: sha, Grant: "read-write",
			Roles:        []contract.ProgramRoleBinding{{Role: "worker", Profile: "worker-profile"}, {Role: "reviewer", Profile: "reviewer-profile"}},
			Capabilities: []string{"workspace-write"}, AllowedPaths: []string{"src/**"}, ProtectedPaths: []string{},
			RequiredChecks: []string{"go-test"}, AllowedChangeKinds: []string{"source"},
			Obligations:  []contract.WorkflowObligation{{ID: "check", Criterion: "The check passes", RequiredChecks: []string{"go-test"}}},
			RepairRoutes: []contract.RepairRoute{}, Resources: []contract.ResourceClaim{},
			Budget: contract.ProgramBudget{Children: 1, RepairCycles: 1, WorkerTurns: 2, AuditAttempts: 1},
		}},
		Escalations: []contract.Escalation{{ID: "scope", When: "scope changes", Description: "ask the operator"}},
	}
}
