package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/minimaxacp"
	"github.com/emjin/agent-engine/internal/runtime"
)

func cliPilotFixture(t *testing.T) (string, string) {
	t.Helper()
	root := episodeFixtureWithTemplate(t)
	path := filepath.Join(root, "CONTROL.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, workspace := readFixtureRoots(t, root)
	text := strings.Replace(string(body), "[profiles.investigator]\nrole = \"worker\"\ntool = \"mcode\"", "[profiles.investigator]\nrole = \"worker\"\ntool = \"mcode\"\ntransport = \"minimax-acp\"\nregion = \"cn\"\nmodel = \"m:minimax:MiniMax-M2.7:v:thinking\"\neffort = \"max\"\npermission_mode = \"bypassPermissions\"", 1)
	// Only the template's read-only declaration is changed; the auditor stays
	// synthetically qualified read-only. This fixture never calls a provider.
	text = strings.Replace(text, "max_repair_iterations = 2\nread_only = \"true\"", "max_repair_iterations = 2\nread_only = \"false\"", 1)
	text += fmt.Sprintf("\n[local_pilots.EP-CLI-PILOT]\nworkspace = %q\nworker_profile = \"investigator\"\nexpires_at = %q\nmax_wall_seconds = 900\nmax_repair_iterations = 1\n", workspace, time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	request := episodeRequestFile(t, workspace, "EP-CLI-PILOT", "forge-investigation")
	body, err = os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(request, []byte(strings.Replace(string(body), `"allow_code_changes": false`, `"allow_code_changes": true`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, root, "episode", "admit", "--request-file", request, "--json")
	if res.code != 0 {
		t.Fatalf("compiled pilot admission: %s", res)
	}
	return root, workspace
}

func TestLocalPilot_CompiledAdmissionAndStatus(t *testing.T) {
	root, _ := cliPilotFixture(t)
	state, err := storeFor(t, root).State("EP-CLI-PILOT")
	if err != nil {
		t.Fatal(err)
	}
	if state.LocalPilot == nil || state.Episode == nil || state.Revision != 2 || state.ExecutionProfiles[control.RoleWorker].Proven {
		t.Fatal("compiled admission lost grant/root or invented proof")
	}
	res := runCLI(t, root, "episode", "status", "--episode-id", state.RequestID, "--json")
	var status map[string]any
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &status) != nil || status["execution_assurance"] != control.AssuranceTrustedLocal {
		t.Fatalf("status hid assurance: %s", res)
	}
}

func TestLocalPilot_ProductionFactoryAppliesSelectionAndPermitsShell(t *testing.T) {
	root, workspace := cliPilotFixture(t)
	// A scripted disposable ACP release exercises the actual pinned factory,
	// process, model/effort setters and shell. It is not live MiniMax evidence.
	bundle := t.TempDir()
	launcher := filepath.Join(bundle, ".mcode-launcher")
	node := filepath.Join(bundle, "bin", "node")
	entry := filepath.Join(bundle, "lib", "node_modules", "@minimax-ai", "code", "cli.js")
	for _, path := range []string{node, entry} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	server := `#!/bin/sh
# pinned fixture Node: 'NODE_PATH'
model='m:minimax:MiniMax-M2.7:v:thinking'
effort=high
permission=bypassPermissions
selected=0
options() {
 printf '{"configOptions":[{"id":"model","type":"select","currentValue":"%s","options":[{"value":"m:minimax:MiniMax-M2.7:v:thinking"}]},{"id":"permissionMode","type":"select","currentValue":"%s","options":[{"value":"bypassPermissions"}]}' "$model" "$permission"
 if [ "$selected" = 1 ]; then printf ',{"id":"thinkingEffort","type":"select","currentValue":"%s","options":[{"value":"high"},{"value":"max"}]}' "$effort"; fi
 printf ']}'
}
while IFS= read -r line; do
 id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
 [ -z "$id" ] && continue
 case "$line" in
  *'"method":"initialize"'*) result='{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fixture","version":"1"}}';;
  *'"method":"session/new"'*) result=$(options | sed 's/^{/{"sessionId":"fixture-1",/');;
  *'"configId":"model"'*) selected=1; result=$(options);;
  *'"configId":"thinkingEffort"'*) effort=max; result=$(options);;
  *'"method":"session/set_config_option"'*) result=$(options);;
  *'"method":"session/prompt"'*) printf 'shell-ok' > shell-output.txt; result='{"stopReason":"end_turn"}';;
  *) result='{}';;
 esac
 printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" "$result"
done
`
	if err := os.WriteFile(launcher, []byte(strings.Replace(server, "NODE_PATH", node, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := minimaxacp.MeasureRelease(launcher)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"release_artifacts": artifacts})
	if err := os.WriteFile(filepath.Join(root, "MINIMAX-RELEASE.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_ENGINE_MCODE_LAUNCHER", launcher)
	st := storeFor(t, root)
	observed := false
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	backends, stop, err := admitPinnedTransportsWithStarter(ctx, st, "EP-CLI-PILOT", workspace, false,
		func(ctx context.Context, opts minimaxacp.Options) (*minimaxacp.Connection, error) {
			if opts.Effort != "max" || opts.Model != "m:minimax:MiniMax-M2.7:v:thinking" || opts.PermissionMode != "bypassPermissions" || opts.Boundary != nil || opts.OnSpawned == nil || opts.HandshakeTimeout != 90*time.Second {
				t.Fatal("production factory dropped selection, denied shell, or omitted ownership callback")
			}
			if opts.TurnTimeout <= minimaxacp.DefaultTurnTimeout || opts.TurnTimeout > 20*time.Minute {
				t.Fatalf("coding turn did not inherit the supervisor deadline: %s", opts.TurnTimeout)
			}
			for _, process := range readOwned(t, root, "EP-CLI-PILOT") {
				if !process.Pending || process.PID != 0 {
					t.Fatal("intent must precede spawn")
				}
			}
			observed = true
			return minimaxacp.Start(ctx, opts)
		})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !observed || len(backends) != 1 {
		t.Fatal("production factory did not reach the real process")
	}
	conn := backends[0].(*minimaxacp.Connection)
	ref, session, err := conn.ActivateResult(context.Background(), runtime.ActivateSpec{Label: "pilot-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if session.Option("thinkingEffort").CurrentValue != "max" {
		t.Fatal("effort was not applied/read back")
	}
	if _, err := conn.Submit(context.Background(), ref, "run the shell control"); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(filepath.Join(workspace, "shell-output.txt"))
	if err != nil || string(output) != "shell-ok" {
		t.Fatalf("real shell failed: %s %v", output, err)
	}
	// Exercise activation/binding/dispatch too: a raw connection submit alone
	// misses the production per-profile backend-name boundary.
	deck := &agentdeck.CLIRuntime{Binary: "/must-not-be-called"}
	engine, err := control.NewEngine(st, deck, "EP-CLI-PILOT", control.EngineOptions{Backends: backends})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	launched, err := engine.LaunchProfile(context.Background(), "EP-CLI-PILOT", control.RoleWorker, "investigator", workspace, "dispatch-composition")
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.State("EP-CLI-PILOT")
	if err != nil {
		t.Fatal(err)
	}
	binding := state.BackendBindings[control.RoleWorker]
	if binding.Backend != conn.Name() || binding.Backend == minimaxacp.BackendName {
		t.Fatalf("binding lost process-specific backend: %+v", binding)
	}
	_, err = st.UpdateCommitted("EP-CLI-PILOT", func(s *control.State) error {
		return s.RecordEffect(control.Effect{EffectID: "profile-dispatch", SessionID: launched.SessionID, Role: "worker", Message: "run shell control", Status: control.EffectPending, Revision: s.Revision, CreatedAt: s.UpdatedAt})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.NewDispatcher(st, deck).WithBackend(conn).Drain(context.Background(), "EP-CLI-PILOT"); err != nil {
		t.Fatal(err)
	}
	state, err = st.State("EP-CLI-PILOT")
	if err != nil {
		t.Fatal(err)
	}
	if state.Effects[len(state.Effects)-1].Status != control.EffectDelivered {
		t.Fatalf("profile-bound prompt never reached actual ACP process: %+v", state.Effects)
	}
	if _, err := conn.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Expiry refuses before reaching the starter, without disabling cleanup.
	_, err = st.UpdateCommitted("EP-CLI-PILOT", func(s *control.State) error { s.LocalPilot.ExpiresAt = "2000-01-01T00:00:00Z"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, refusedStop, err := admitPinnedTransportsWithStarter(context.Background(), st, "EP-CLI-PILOT", workspace, false,
		func(context.Context, minimaxacp.Options) (*minimaxacp.Connection, error) {
			t.Fatal("expired grant reached spawn")
			return nil, nil
		})
	defer refusedStop()
	if err == nil {
		t.Fatal("expired authorization admitted")
	}
}

func TestLocalPilot_ExpiredCancellationDoesNotLaunch(t *testing.T) {
	testExpiredPilotRecovery(t, true)
}

func TestLocalPilot_ExpiredEpisodeRecordsFailureWithoutLaunch(t *testing.T) {
	testExpiredPilotRecovery(t, false)
}

func testExpiredPilotRecovery(t *testing.T, cancellation bool) {
	t.Helper()
	root, _ := cliPilotFixture(t)
	// The reviewed fixture runtime is executable but must never be called: this
	// request owns no sessions, and cancelling it requires no new execution.
	deck := filepath.Join(t.TempDir(), "fixture-deck")
	if err := os.WriteFile(deck, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	digest, err := runtime.FileSHA256(deck)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "runtime-lock.json")
	lockBody, err := json.Marshal(map[string]any{"schema": 1, "repository": "https://github.com/asheshgoplani/agent-deck.git", "source_commit": strings.Repeat("a", 40), "artifacts": map[string]any{goruntime.GOOS + "/" + goruntime.GOARCH: map[string]string{"path": deck, "sha256": digest}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, lockBody, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_ENGINE_RUNTIME_LOCK", lockPath)
	configPath := filepath.Join(root, "CONTROL.toml")
	configBody, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configBody = []byte(strings.Replace(string(configBody), `agent_deck_binary = "agent-deck"`, "agent_deck_binary = "+fmt.Sprintf("%q", deck), 1))
	if err := os.WriteFile(configPath, configBody, 0600); err != nil {
		t.Fatal(err)
	}
	st := storeFor(t, root)
	_, err = st.UpdateCommitted("EP-CLI-PILOT", func(s *control.State) error {
		s.LocalPilot.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		s.Episode.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		s.CancellationRequested = cancellation
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// No release receipt/launcher is installed. Any ACP initialization would
	// refuse before completing cancellation; cleanup must run without it.
	res := runCLI(t, root, "run", "--request-id", "EP-CLI-PILOT", "--json")
	expected := control.StageCancelled
	if cancellation {
		if res.code != 0 {
			t.Fatalf("expired cancellation: %s", res)
		}
	} else {
		expected = control.StageBlocked
		if res.code == 0 {
			t.Fatalf("expired execution was allowed: %s", res)
		}
	}
	s, err := st.State("EP-CLI-PILOT")
	if err != nil || s.Stage != expected || len(s.OwnedProcesses) != 0 {
		t.Fatalf("cancellation started a process or failed to settle: state=%+v err=%v", s, err)
	}
}

// An exhausted supervisor must refuse before recording spawn intent or reaching
// a launcher. No model or fixture process is started by these cases.
func TestACPFactory_ExhaustedContextDoesNotStart(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "cancelled"
		if expired {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			want := error(context.Canceled)
			if expired {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}
			defer cancel()
			started := false
			_, err := startACPInstanceWithEffort(ctx, nil, "", "", "", "", "", "", func(context.Context, minimaxacp.Options) (*minimaxacp.Connection, error) {
				started = true
				return nil, nil
			})
			if !errors.Is(err, want) || started {
				t.Fatalf("exhausted context reached startup: started=%v err=%v", started, err)
			}
		})
	}
}
