package control

// Episode admission role binding tests.
//
// The claim under test has a refusal half and a wiring half, and the refusal
// half is the one that matters:
//
//   - admitting an episode binds every role its approved template names, through
//     the same ResolveProfile the launch path uses, so an approved shape is also
//     a runnable one;
//   - a template naming a role this operator cannot actually serve is REFUSED,
//     and commits nothing at all — not a partial binding, not an admitted request
//     with no roles.
//
// "Commits nothing" is the load-bearing part. An episode recorded without a
// binding looks identical to one whose binding was forgotten, and every reader
// of ExecutionProfiles answers "not found" in both cases.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
)

// episodeProfiles is the operator-approved registry these tests bind against.
//
// Read-only confinement is synthetic fixture evidence. Refusal tests explicitly
// remove it; this registry is never installed as operator configuration.
func episodeProfiles() map[string]Profile {
	return map[string]Profile{
		"impl":    {Name: "impl", Role: RoleWorker, Tool: "mcode", Model: "worker-model"},
		"audit":   {Name: "audit", Role: RoleReviewer, Tool: "mcode", ReadOnly: true, Proven: true},
		"gate":    {Name: "gate", Role: RoleCheckpoint, Tool: "mcode"},
		"triage":  {Name: "triage", Role: RoleDiagnostic, Tool: "mcode"},
		"looksee": {Name: "looksee", Role: RoleInvestigat, Tool: "mcode", ReadOnly: true, Proven: true},
	}
}

// episodeTemplateWith is a valid template bound to the roles given.
func episodeTemplateWith(roles map[ProfileRole]string) EpisodeTemplate {
	t := fullTemplate()
	t.Roles = roles
	return t
}

// episodeBoundFixture is a store whose operator has approved profiles and one
// episode template.
func episodeBoundFixture(t *testing.T, roles map[ProfileRole]string) *fixture {
	t.Helper()
	f := newFixture(t)
	f.cfg.Profiles = episodeProfiles()
	tpl := episodeTemplateWith(roles)
	if err := f.cfg.EpisodeTemplates.Register(tpl); err != nil {
		t.Fatalf("register template: %v", err)
	}
	return f
}

// episodeRequest is an admissible episode under the standard_fix template.
func episodeRequest(f *fixture, id string) *contract.EpisodeRequest {
	abs, _ := filepath.Abs(filepath.Join(f.srcRoot, "project"))
	return &contract.EpisodeRequest{
		Version:   contract.EpisodeVersion,
		EpisodeID: id,
		Origin:    contract.Origin{Kind: "forge", CallerID: "local-forge"},
		Workspace: contract.Workspace{Path: abs, ExpectedHead: "abc123"},
		Objective: "Fix the attributable regression without changing protected evaluation code",
		Template:  "standard_fix",
		Obligations: []contract.WorkflowObligation{{
			ID: "O-1", Criterion: "Regression reproduced, fixed and verified",
			RequiredChecks: []string{"build", "regression"},
		}},
		Constraints: contract.Constraints{
			AllowCodeChanges: true,
			AllowedPaths:     []string{"src/**"},
			ProtectedPaths:   []string{"evaluation/**"},
		},
		Verification: []contract.VerificationItem{{CheckID: "build"}, {CheckID: "regression"}},
		Limits:       contract.Limits{RepairCycles: 3, WorkerGens: 2, AuditAttempts: 2},
	}
}

// allEpisodeRoles binds every role the contract defines, so a missing one is a
// bug in the binding and not in the fixture.
func allEpisodeRoles() map[ProfileRole]string {
	return map[ProfileRole]string{
		RoleWorker:     "impl",
		RoleReviewer:   "audit",
		RoleCheckpoint: "gate",
		RoleDiagnostic: "triage",
		RoleInvestigat: "looksee",
	}
}

func TestEpisodeAdmission_ReadOnlyRoleCannotOmitConfinement(t *testing.T) {
	for _, role := range []ProfileRole{RoleReviewer, RoleInvestigat} {
		t.Run(string(role), func(t *testing.T) {
			f := episodeBoundFixture(t, allEpisodeRoles())
			name := allEpisodeRoles()[role]
			profile := f.cfg.Profiles[name]
			profile.ReadOnly, profile.Proven = false, false
			f.cfg.Profiles[name] = profile
			if _, err := f.store.AdmitEpisode(episodeRequest(f, "EP-OMITTED-CONFINEMENT")); !errors.Is(err, ErrConfinementUnproven) {
				t.Fatalf("omitted confinement was not refused: %v", err)
			}
			if _, err := f.store.State("EP-OMITTED-CONFINEMENT"); err == nil {
				t.Fatal("refused role committed an episode")
			}
		})
	}
}

// TestEpisodeAdmission_BindsEveryTemplateRole asserts the wiring half.
//
// The stored identity must be the RESOLVED profile, not the name: an operator
// editing what "impl" means afterwards must not re-point a running episode.
func TestEpisodeAdmission_BindsEveryTemplateRole(t *testing.T) {
	f := episodeBoundFixture(t, allEpisodeRoles())
	if _, err := f.store.AdmitEpisode(episodeRequest(f, "EP-BIND")); err != nil {
		t.Fatalf("admit episode: %v", err)
	}

	st, err := f.store.State("EP-BIND")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.ExecutionProfiles) != len(allEpisodeRoles()) {
		t.Fatalf("bound %d roles, want %d: %+v", len(st.ExecutionProfiles), len(allEpisodeRoles()), st.ExecutionProfiles)
	}
	for role, name := range allEpisodeRoles() {
		got, ok := st.ExecutionProfiles[role]
		if !ok {
			t.Errorf("role %s was not bound", role)
			continue
		}
		want := f.cfg.Profiles[name].Ref()
		if got.Name != want.Name || got.Role != want.Role || got.Model != want.Model || got.Tool != want.Tool {
			t.Errorf("role %s bound %+v, want the resolved identity %+v", role, got, want)
		}
	}

	// The binding is pinned, so re-pointing the registry cannot re-point the
	// episode that is already admitted.
	f.cfg.Profiles["impl"] = Profile{Name: "impl", Role: RoleWorker, Tool: "mcode", Model: "swapped"}
	again, err := f.store.State("EP-BIND")
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if again.ExecutionProfiles[RoleWorker].Model != "worker-model" {
		t.Errorf("the binding followed the registry instead of the pin: %q",
			again.ExecutionProfiles[RoleWorker].Model)
	}

	// PinnedProfiles is a LAUNCH record — state.go says so, and activation.go is
	// its only writer. Admission pins an approval, so it must not append here:
	// a role that never ran would be indistinguishable from one that did.
	if len(again.PinnedProfiles) != 0 {
		t.Errorf("admission wrote launch records for sessions that never started: %+v", again.PinnedProfiles)
	}
}

// TestEpisodeAdmission_UnresolvableRoleIsRefusedAndCommitsNothing asserts the
// refusal half.
//
// The unresolvable role is the WORKER, which sorts last, so four roles resolve
// before the refusal. That ordering is deliberate: it is the case where a
// partial binding would be built and then thrown away, and a caller that kept
// it would commit a state whose reviewer is bound and whose worker is missing.
func TestEpisodeAdmission_UnresolvableRoleIsRefusedAndCommitsNothing(t *testing.T) {
	cases := []struct {
		name string
		role map[ProfileRole]string
		want error
		says string
	}{
		{
			name: "an unapproved name",
			role: map[ProfileRole]string{RoleWorker: "no-such-profile", RoleReviewer: "audit"},
			want: ErrUnknownProfile,
			says: "operator has approved",
		},
		{
			// A profile that EXISTS and is approved — for the other role. This
			// is the case a hand-rolled map read would accept silently, and it
			// is the one that would run the implementer as the reviewer.
			name: "a profile approved for another role",
			role: map[ProfileRole]string{RoleWorker: "impl", RoleReviewer: "gate"},
			want: ErrRoleMismatch,
			says: "not reviewer",
		},
		{
			// ACP is worker-only. The role is otherwise perfectly approved.
			name: "a transport that cannot serve the role",
			role: map[ProfileRole]string{RoleWorker: "impl", RoleReviewer: "acp-reviewer"},
			want: ErrTransportUnsupported,
			says: "cannot serve",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profiles := episodeProfiles()
			profiles["acp-reviewer"] = Profile{
				Name: "acp-reviewer", Role: RoleReviewer, Transport: TransportMiniMaxACP, Tool: "mcode",
			}
			f := episodeBoundFixture(t, tc.role)
			f.cfg.Profiles = profiles

			_, err := f.store.AdmitEpisode(episodeRequest(f, "EP-REFUSED"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("the episode was not refused with %v: %v", tc.want, err)
			}
			if !errors.Is(err, contract.ErrEpisodeInvalid) {
				t.Errorf("the refusal does not say the episode is invalid: %v", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say why: %v", err)
			}
			// Nothing may exist: not a phase root, not a partial binding.
			if _, serr := f.store.State("EP-REFUSED"); serr == nil {
				t.Fatal("a refused episode left a request on disk; a binding refusal must commit nothing")
			}
		})
	}
}

// TestEpisodeAdmission_UnresolvableRoleRefusalIsDeterministic guards the one
// place map iteration could leak.
//
// The roles are resolved in sorted order, so a template with two unresolvable
// roles names the SAME one every run. Without sorting this test fails about half
// the time, which is the worst possible failure mode for a refusal.
func TestEpisodeAdmission_UnresolvableRoleRefusalIsDeterministic(t *testing.T) {
	f := episodeBoundFixture(t, map[ProfileRole]string{
		RoleWorker: "ghost-worker", RoleReviewer: "ghost-reviewer", RoleDiagnostic: "ghost-triage",
	})
	ep := episodeRequest(f, "EP-ORDER")

	_, first := f.store.AdmitEpisode(ep)
	if first == nil {
		t.Fatal("two unresolvable roles were accepted")
	}
	for i := 0; i < 8; i++ {
		_, again := f.store.AdmitEpisode(ep)
		if again.Error() != first.Error() {
			t.Fatalf("refusal %d differs from the first:\n first: %v\n  again: %v", i, first, again)
		}
	}
	// "checkpoint" would sort first and is absent from this template; "diagnostic"
	// is the lowest role present, and all three names sort before "worker".
	if !strings.Contains(first.Error(), "diagnostic") {
		t.Errorf("the refusal did not name the lowest-sorting unresolvable role: %v", first)
	}
}

// TestEpisodeAdmission_UnprovenReadOnlyReviewerIsRefused is the confinement
// refusal, at admission rather than at launch.
//
// A reviewer that declares read-only is asserting a write restriction it has not
// demonstrated. Binding it would make the episode look auditable while its
// auditor's confinement is a prompt, and every later reader — activation's
// capability check included — trusts the binding rather than re-deriving it.
func TestEpisodeAdmission_UnprovenReadOnlyReviewerIsRefused(t *testing.T) {
	f := episodeBoundFixture(t, map[ProfileRole]string{RoleWorker: "impl", RoleReviewer: "audit"})
	// confinement_proven = "false": declared read-only, nothing demonstrated.
	f.cfg.Profiles["audit"] = Profile{
		Name: "audit", Role: RoleReviewer, Tool: "mcode",
		ReadOnly: true, Sandbox: true, Proven: false,
	}

	_, err := f.store.AdmitEpisode(episodeRequest(f, "EP-CONFINEMENT"))
	if !errors.Is(err, ErrConfinementUnproven) {
		t.Fatalf("an unproven read-only reviewer was bound: %v", err)
	}
	if !strings.Contains(err.Error(), "prompt, not a boundary") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if _, serr := f.store.State("EP-CONFINEMENT"); serr == nil {
		t.Fatal("a refused episode was committed anyway")
	}

	// The gate is about evidence, not a permanent ban: once the operator records
	// the demonstration the same template admits and binds.
	f.cfg.Profiles["audit"] = Profile{
		Name: "audit", Role: RoleReviewer, Tool: "mcode",
		ReadOnly: true, Sandbox: true, Proven: true,
	}
	if _, aerr := f.store.AdmitEpisode(episodeRequest(f, "EP-CONFINEMENT")); aerr != nil {
		t.Fatalf("a demonstrated profile was still refused: %v", aerr)
	}
	st, serr := f.store.State("EP-CONFINEMENT")
	if serr != nil {
		t.Fatalf("load state: %v", serr)
	}
	if ref := st.ExecutionProfiles[RoleReviewer]; !ref.Proven || !ref.ReadOnly {
		t.Errorf("the reviewer was bound without its confinement evidence: %+v", ref)
	}
}
