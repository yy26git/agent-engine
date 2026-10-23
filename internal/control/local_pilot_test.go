package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// These backend-composition fixtures deliberately authorize a disposable local
// pilot. They test activation/reconciliation, not real MiniMax confinement.
func grantActivationFixture(t *testing.T, s *State, workspace string, worker Profile) {
	t.Helper()
	root, err := canonicalPilotWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	s.Workspace = root
	now := time.Now()
	g := LocalPilotGrant{RequestID: s.RequestID, Workspace: root, WorkerProfile: worker.Name,
		ExpiresAt: now.Add(15 * time.Minute).UTC().Format(time.RFC3339), MaxWallSeconds: 900,
		MaxRepairIterations: 1, ProfileSHA256: profileDigest(worker)}
	g.SHA256 = g.digest()
	s.LocalPilot = &g
	s.ExecutionAssurance = AssuranceTrustedLocal
	budget := DefaultEpisodeBudget()
	budget.WallSeconds = 900
	budget.RepairIterations = 1
	if err := s.StartEpisode(s.RequestID, budget, now); err != nil {
		t.Fatal(err)
	}
}

func pilotFixture(t *testing.T, id string) (*fixture, *contract.EpisodeRequest) {
	t.Helper()
	f := episodeBoundFixture(t, allEpisodeRoles())
	ep := episodeRequest(f, id)
	if err := os.MkdirAll(ep.Workspace.Path, 0700); err != nil {
		t.Fatal(err)
	}
	p := f.cfg.Profiles["impl"]
	p.Transport, p.Tool, p.Model = TransportMiniMaxACP, "mcode", "m:minimax:MiniMax-M2.7:v:thinking"
	f.cfg.Profiles["impl"] = p
	f.cfg.LocalPilots = map[string]LocalPilotGrant{id: {
		RequestID: id, Workspace: ep.Workspace.Path, WorkerProfile: "impl",
		ExpiresAt: f.now.Add(15 * time.Minute).Format(time.RFC3339), MaxWallSeconds: 900, MaxRepairIterations: 1,
	}}
	return f, ep
}

func TestLocalPilot_AtomicAdmissionPinsGrantAndPreservesDuplicate(t *testing.T) {
	f, ep := pilotFixture(t, "EP-PILOT")
	adm, err := f.store.AdmitEpisode(ep)
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !adm.Accepted || s.Revision != 2 || s.Episode == nil || len(s.ExecutionProfiles) != len(allEpisodeRoles()) ||
		s.LocalPilot == nil || s.ExecutionAssurance != AssuranceTrustedLocal || s.LocalPilot.SHA256 != s.LocalPilot.digest() {
		t.Fatalf("initial commit omitted pilot/episode/roles: %+v", s)
	}
	worker := s.ExecutionProfiles[RoleWorker].Profile()
	if worker.Proven || s.Episode.Budget.WallSeconds != 900 || s.Episode.Budget.RepairIterations != 1 || s.Limits.RepairCycles != 1 {
		t.Fatal("pilot invented proof or failed to cap budget")
	}
	if err := s.AuthorizeProfileExecution(worker, s.Workspace, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error { s.Episode.Attempt = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.State(ep.EpisodeID)
	duplicate, err := f.store.AdmitEpisode(ep)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate: %v %+v", err, duplicate)
	}
	after, _ := f.store.State(ep.EpisodeID)
	if after.Revision != before.Revision || after.Episode.Attempt != 2 {
		t.Fatal("duplicate rewrote/reset episode")
	}
	copy := after.Clone()
	copy.LocalPilot.WorkerProfile = "other"
	if after.LocalPilot.WorkerProfile != "impl" {
		t.Fatal("grant clone aliases state")
	}
}

func TestLocalPilot_ScopeAndReviewerRefusalsCommitNothing(t *testing.T) {
	for _, change := range []string{"missing", "expired", "workspace", "worker", "budget", "reviewer"} {
		t.Run(change, func(t *testing.T) {
			f, ep := pilotFixture(t, "EP-REFUSED")
			g := f.cfg.LocalPilots[ep.EpisodeID]
			switch change {
			case "missing":
				delete(f.cfg.LocalPilots, ep.EpisodeID)
			case "expired":
				g.ExpiresAt = f.now.Format(time.RFC3339)
			case "workspace":
				g.Workspace = f.srcRoot
			case "worker":
				g.WorkerProfile = "looksee"
			case "budget":
				g.MaxRepairIterations = 999
			case "reviewer":
				p := f.cfg.Profiles["audit"]
				p.Proven = false
				f.cfg.Profiles["audit"] = p
			}
			if change != "missing" {
				f.cfg.LocalPilots[ep.EpisodeID] = g
			}
			if _, err := f.store.AdmitEpisode(ep); err == nil {
				t.Fatal("invalid pilot admitted")
			}
			if _, err := os.Stat(filepath.Join(f.root, RequestsDirName, ep.EpisodeID)); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote request: %v", err)
			}
		})
	}
}

func TestLocalPilot_FrozenIdentityAndLaunchGates(t *testing.T) {
	f, ep := pilotFixture(t, "EP-LAUNCH")
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	s, _ := f.store.State(ep.EpisodeID)
	worker := s.ExecutionProfiles[RoleWorker].Profile()
	p := f.cfg.Profiles["impl"]
	p.Model = "changed-model"
	f.cfg.Profiles["impl"] = p
	delete(f.cfg.LocalPilots, ep.EpisodeID)
	if duplicate, err := f.store.AdmitEpisode(ep); err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate inherited changed live grant/model: %v", err)
	}
	if err := s.AuthorizeProfileExecution(worker, s.Workspace, f.now); err != nil {
		t.Fatalf("followed live config: %v", err)
	}
	for _, change := range []string{"model", "workspace", "expiry", "budget", "digest", "old-state"} {
		t.Run(change, func(t *testing.T) {
			state := s.Clone()
			candidate := worker
			workspace := s.Workspace
			now := f.now
			switch change {
			case "model":
				candidate.Model = "unadmitted"
			case "workspace":
				workspace = f.srcRoot
			case "expiry":
				now = f.now.Add(15 * time.Minute)
			case "budget":
				state.Episode.Budget.WallSeconds = 901
			case "digest":
				state.LocalPilot.SHA256 = "asserted"
			case "old-state":
				state.LocalPilot = nil
			}
			if err := state.AuthorizeProfileExecution(candidate, workspace, now); err == nil {
				t.Fatal("invalid launch authorized")
			}
		})
	}
	// The public pinned Engine seam also refuses before recording an activation.
	eng, close := episodeEngine(t, f, newFakeRuntime(), ep.EpisodeID)
	defer close()
	if _, err := eng.LaunchProfile(context.Background(), ep.EpisodeID, RoleWorker, "impl", f.srcRoot, "test"); err == nil {
		t.Fatal("Engine launch ignored grant scope")
	}
	after, _ := f.store.State(ep.EpisodeID)
	if len(after.Activations) != 0 {
		t.Fatal("refused Engine launch committed activation")
	}
}

func TestLocalPilot_EpisodeTemplateIdentityCannotBeRebound(t *testing.T) {
	f, ep := pilotFixture(t, "EP-IDENTITY")
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	tpl := episodeTemplateWith(allEpisodeRoles())
	tpl.Name = "different"
	if err := f.cfg.EpisodeTemplates.Register(tpl); err != nil {
		t.Fatal(err)
	}
	ep.Template = tpl.Name
	if _, err := f.store.AdmitEpisode(ep); !errors.Is(err, ErrConflict) {
		t.Fatalf("template rebound same episode: %v", err)
	}
}

func TestLocalPilot_CallerCannotSupplyGrant(t *testing.T) {
	_, err := contract.DecodeEpisodeRequest([]byte(`{"version":1,"episode_id":"caller","template":"fix","objective":"fix","obligations":[{"id":"O","criterion":"fixed"}],"local_pilot":{"enabled":true}}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("caller could supply pilot: %v", err)
	}
}

func TestLocalPilot_ConfigParser(t *testing.T) {
	f := newFixture(t)
	raw, err := os.ReadFile(filepath.Join(f.root, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	grant := `
[local_pilots.PILOT]
workspace = "` + f.srcRoot + `"
worker_profile = "worker"
expires_at = "2026-12-01T00:00:00Z"
max_wall_seconds = 900
max_repair_iterations = 1
`
	for _, invalid := range []string{"", "max_wall_seconds = 900", "max_repair_iterations = 1", "expires_at = \"2026-12-01T00:00:00Z\""} {
		text := grant
		if invalid != "" {
			text = strings.Replace(text, invalid, "unknown = 1", 1)
		}
		cfg, err := parseConfig(append(append([]byte(nil), raw...), []byte(text)...))
		if invalid == "" {
			if err != nil || cfg.LocalPilots["PILOT"].MaxWallSeconds != 900 {
				t.Fatalf("valid local grant: %v", err)
			}
		} else if err == nil {
			t.Fatalf("unknown grant accepted: %s", invalid)
		}
	}
}

func TestLocalPilot_TemplateWallAllowanceIsPinnedAndCapped(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		approved, grant, want int
	}{
		{"legacy-default", 0, 10800, 3600},
		{"explicit-long", 10800, 10800, 10800},
		{"grant-caps", 10800, 1800, 1800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ep := pilotFixture(t, "EP-WALL")
			tpl, _ := f.cfg.EpisodeTemplates.Lookup(ep.Template)
			tpl.MaxWallSeconds = tc.approved
			reg := NewEpisodeTemplateRegistry()
			if err := reg.Register(tpl); err != nil {
				t.Fatal(err)
			}
			f.cfg.EpisodeTemplates = reg
			g := f.cfg.LocalPilots[ep.EpisodeID]
			g.MaxWallSeconds = tc.grant
			g.ExpiresAt = f.now.Add(24 * time.Hour).Format(time.RFC3339)
			f.cfg.LocalPilots[ep.EpisodeID] = g
			if _, err := f.store.AdmitEpisode(ep); err != nil {
				t.Fatal(err)
			}
			s, err := f.store.State(ep.EpisodeID)
			if err != nil {
				t.Fatal(err)
			}
			if s.Episode.Budget.WallSeconds != tc.want {
				t.Fatalf("wall=%d want=%d", s.Episode.Budget.WallSeconds, tc.want)
			}
			// Later configuration cannot extend an already admitted execution deadline.
			tpl.MaxWallSeconds = 86400
			reg = NewEpisodeTemplateRegistry()
			if err := reg.Register(tpl); err != nil {
				t.Fatal(err)
			}
			f.cfg.EpisodeTemplates = reg
			if _, err := f.store.AdmitEpisode(ep); err != nil {
				t.Fatal(err)
			}
			s, _ = f.store.State(ep.EpisodeID)
			if s.Episode.Budget.WallSeconds != tc.want {
				t.Fatal("duplicate admission changed immutable budget")
			}
		})
	}
}
