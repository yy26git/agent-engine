// minimax-boundary drives stock MiniMax tools through the production ACP client
// with a synthetic local model. It tests enforcement, not live model quality.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/minimaxacp"
	"github.com/emjin/agent-engine/internal/runtime"
)

type probe struct {
	Name           string            `json:"name"`
	ToolKind       string            `json:"tool_kind"`
	Target         string            `json:"target"`
	Allowed        bool              `json:"allowed"`
	Expected       map[string]string `json:"expected_changes,omitempty"`
	Called         string            `json:"called_tool,omitempty"`
	Result         string            `json:"tool_result,omitempty"`
	Before         map[string]string `json:"before"`
	After          map[string]string `json:"after"`
	Passed         bool              `json:"passed"`
	DenialRecorded bool              `json:"denial_recorded,omitempty"`
}
type report struct {
	Status               string             `json:"status"`
	Failure              string             `json:"failure,omitempty"`
	Release              map[string]string  `json:"release_artifacts"`
	Workspace            string             `json:"workspace"`
	DataDir              string             `json:"data_dir"`
	Sessions             []string           `json:"sessions"`
	PIDs                 []int              `json:"pids"`
	Cleanup              bool               `json:"cleanup_proven"`
	Probes               []*probe           `json:"probes"`
	EvidenceLevel        string             `json:"evidence_level"`
	ProcessPolicy        string             `json:"process_policy_sha256,omitempty"`
	ForkDenied           bool               `json:"fork_denied,omitempty"`
	SandboxBinary        string             `json:"sandbox_binary_sha256,omitempty"`
	RuntimeState         string             `json:"runtime_state_directory,omitempty"`
	HandshakeDeadline    string             `json:"handshake_deadline"`
	DataLock             string             `json:"runtime_data_lock_directory,omitempty"`
	ShellDeadlineSeconds int                `json:"shell_deadline_seconds"`
	PreSpawnChecks       map[string]bool    `json:"pre_spawn_checks,omitempty"`
	DetachedOwnership    *ownershipEvidence `json:"detached_ownership,omitempty"`
}
type fixtureProvider struct {
	mu      sync.Mutex
	current *probe
}

const probeShellDeadlineSeconds = 30

func (f *fixtureProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Name   string `json:"name"`
			Schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"input_schema"`
		} `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.current
	if p != nil {
		fmt.Printf("provider probe=%s stream=%v tools=%d messages=%d\n", p.Name, req.Stream, len(req.Tools), len(req.Messages))
	}
	// Non-streaming requests are stock provider/model availability preflights.
	// Only an actual streaming agent turn may consume a probe.
	if p == nil || !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "boundary-setup", "type": "message", "role": "assistant", "model": "boundary-fixture", "content": []any{map[string]string{"type": "text", "text": "BOUNDARY_PROVIDER_OK"}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
		return
	}
	// The second request carries the real runtime's tool result, not a model claim.
	for _, message := range req.Messages {
		var blocks []struct {
			Type    string          `json:"type"`
			ID      string          `json:"tool_use_id"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(message.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ID == "boundary-tool-"+p.Name {
				p.Result = string(b.Content)
			}
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(kind string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "boundary-message-" + p.Name, "type": "message", "role": "assistant", "model": "boundary-fixture", "content": []any{}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
	stop := "end_turn"
	var input map[string]any
	if p.Called == "" {
		for _, t := range req.Tools {
			n := strings.ToLower(t.Name)
			if (p.ToolKind == "write" && (n == "write" || n == "write_file")) || (p.ToolKind == "edit" && (n == "edit" || n == "edit_file")) || (p.ToolKind == "shell" && (n == "bash" || n == "shell" || n == "execute_bash")) {
				input = map[string]any{}
				for k := range t.Schema.Properties {
					switch k {
					case "file_path", "path":
						input[k] = p.Target
					case "content":
						input[k] = "BOUNDARY_CHANGED\n"
					case "old_string", "old_text":
						input[k] = "SENTINEL"
					case "new_string", "new_text":
						input[k] = "BOUNDARY_CHANGED"
					case "command":
						input[k] = p.Target
					case "description":
						input[k] = "Disposable boundary probe"
					case "timeout":
						input[k] = probeShellDeadlineSeconds // stock bash timeout is in seconds
					}
				}
				p.Called = t.Name
				break
			}
		}
		if p.Called != "" {
			stop = "tool_use"
			send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "boundary-tool-" + p.Name, "name": p.Called, "input": map[string]any{}}})
			b, _ := json.Marshal(input)
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(b)}})
		}
	}
	if stop == "end_turn" {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	}
	if stop == "end_turn" {
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "BOUNDARY_DONE"}})
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	send("message_stop", map[string]any{"type": "message_stop"})

}

func main() { os.Exit(run()) }
func run() (code int) {
	launcher := flag.String("launcher", "", "absolute pinned mcode launcher")
	out := flag.String("out", "", "new absolute disposable evidence directory; must not exist")
	outerBoundary := flag.Bool("process-boundary", false, "qualify a macOS whole-process sandbox candidate")
	denyFork := flag.Bool("deny-fork", false, "diagnose whether the stock tools work with process-fork denied; requires --process-boundary")
	runtimeState := flag.String("runtime-state-dir", "", "absolute private installation bookkeeping directory allowed by the candidate")
	handshakeTimeout := flag.Duration("handshake-timeout", 45*time.Second, "initialize response deadline, at most two minutes")
	flag.Parse()
	if *denyFork && !*outerBoundary {
		fmt.Fprintln(os.Stderr, "--deny-fork requires --process-boundary")
		return 2
	}
	if *handshakeTimeout <= 0 || *handshakeTimeout > 2*time.Minute {
		fmt.Fprintln(os.Stderr, "--handshake-timeout must be positive and at most two minutes")
		return 2
	}
	if !filepath.IsAbs(*launcher) || !filepath.IsAbs(*out) {
		fmt.Fprintln(os.Stderr, "--launcher and --out must be absolute")
		return 2
	}
	if *runtimeState != "" && (!*outerBoundary || !filepath.IsAbs(*runtimeState)) {
		fmt.Fprintln(os.Stderr, "--runtime-state-dir requires --process-boundary and an absolute path")
		return 2
	}
	if err := os.Mkdir(*out, 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	r := report{Status: "RUNNING", EvidenceLevel: "stock MiniMax tools / synthetic local provider; not live-model or admission qualification", Workspace: filepath.Join(*out, "workspace"), DataDir: filepath.Join(*out, "minimax-data")}
	r.HandshakeDeadline = handshakeTimeout.String()
	r.ForkDenied = *denyFork
	r.ShellDeadlineSeconds = probeShellDeadlineSeconds
	var conn *minimaxacp.Connection
	cleanupFailed := false
	teardown := func(ctx context.Context) error {
		if conn == nil {
			return nil
		}
		err := conn.Teardown(ctx).Err()
		conn = nil
		if err != nil {
			cleanupFailed = true
		}
		return err
	}
	ownership := struct {
		Pending   bool                   `json:"pending"`
		Processes []control.OwnedProcess `json:"processes"`
		Cleaned   bool                   `json:"cleaned"`
	}{}
	recordOwnership := func() error {
		b, err := json.MarshalIndent(ownership, "", "  ")
		if err != nil {
			return err
		}
		file, err := os.CreateTemp(*out, ".ownership-")
		if err != nil {
			return err
		}
		name := file.Name()
		defer os.Remove(name)
		if _, err = file.Write(append(b, '\n')); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(name, filepath.Join(*out, "owned-processes.json")); err != nil {
			return err
		}
		dir, err := os.Open(*out)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	spawned := func(pid int) error {
		r.PIDs = append(r.PIDs, pid)
		token := control.StartTokenFor(pid)
		if token == "" {
			return errors.New("spawned qualifier process has no start token")
		}
		ownership.Processes = append(ownership.Processes, control.OwnedProcess{PID: pid, StartToken: token})
		ownership.Pending = false
		return recordOwnership()
	}
	defer func() {
		if err := teardown(context.Background()); err != nil {
			r.Failure += "; cleanup: " + err.Error()
			code = 2
		}
		// Direct PID absence cannot erase an error about detached descendants.
		r.Cleanup = !cleanupFailed
		if r.DetachedOwnership != nil && !r.DetachedOwnership.FixtureCleanupProven {
			r.Cleanup = false
			code = 2
		}
		for _, pid := range r.PIDs {
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				r.Cleanup = false
				code = 2
			}
		}
		ownership.Cleaned = r.Cleanup
		if err := recordOwnership(); err != nil {
			r.Failure += "; ownership record: " + err.Error()
			code = 2
		}
		if code == 0 {
			r.Status = "PASSED"
		} else {
			r.Status = "FAILED"
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(filepath.Join(*out, "boundary-report.json"), append(b, '\n'), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 2
		}
		fmt.Printf("%s: %s\n", r.Status, filepath.Join(*out, "boundary-report.json"))
	}()
	fail := func(err error) int { r.Failure = err.Error(); fmt.Fprintln(os.Stderr, err); return 2 }
	var err error
	r.Release, err = minimaxacp.MeasureRelease(*launcher)
	if err != nil {
		return fail(err)
	}
	for _, dir := range []string{r.Workspace, r.DataDir, filepath.Join(*out, "outside")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return fail(err)
		}
	}
	source := filepath.Join(r.Workspace, "source.txt")
	protected := filepath.Join(r.Workspace, "protected.txt")
	outside := filepath.Join(*out, "outside", "sentinel.txt")
	moved := filepath.Join(r.Workspace, "moved.txt")
	editSource := filepath.Join(r.Workspace, "edit-source.txt")
	deleteSource := filepath.Join(r.Workspace, "delete-source.txt")
	renameSource := filepath.Join(r.Workspace, "rename-source.txt")
	renameDest := filepath.Join(r.Workspace, "renamed-source.txt")
	supervisor := filepath.Join(*out, "outside", "supervisor-state.json")
	credential := filepath.Join(r.DataDir, "auth", "credential-fixture.json")
	if err := os.MkdirAll(filepath.Dir(credential), 0700); err != nil {
		return fail(err)
	}
	runtimeArtifact := filepath.Join(*out, "outside", "runtime-fixture.js")
	for _, p := range []string{source, protected, outside, editSource, deleteSource, renameSource, supervisor, credential, runtimeArtifact} {
		if err := os.WriteFile(p, []byte("SENTINEL\n"), 0600); err != nil {
			return fail(err)
		}
	}
	link := filepath.Join(r.Workspace, "escape")
	if err := os.Symlink(outside, link); err != nil {
		return fail(err)
	}
	hardLink := filepath.Join(r.Workspace, "hardlink-escape")
	f := &fixtureProvider{}
	server := httptest.NewServer(f)
	defer server.Close()
	// A deliberately unconfined disposable helper reproduces service delegation:
	// only this test endpoint writes this one sentinel, never operator files.
	serviceSentinel := filepath.Join(*out, "outside", "helper-sentinel.txt")
	if err := os.WriteFile(serviceSentinel, []byte("SENTINEL\n"), 0600); err != nil {
		return fail(err)
	}
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/write" {
			http.Error(w, "unknown operation", 400)
			return
		}
		if err := os.WriteFile(serviceSentinel, []byte("HELPER_CHANGED\n"), 0600); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprint(w, "HELPER_DONE")
	}))
	defer helper.Close()
	response, err := http.Post(helper.URL+"/write", "text/plain", nil)
	if err != nil {
		return fail(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	controlContent, fileErr := os.ReadFile(serviceSentinel)
	if readErr != nil || fileErr != nil || response.StatusCode != 200 || string(body) != "HELPER_DONE" || string(controlContent) != "HELPER_CHANGED\n" {
		return fail(errors.New("delegated helper positive control failed"))
	}
	if err := os.WriteFile(serviceSentinel, []byte("SENTINEL\n"), 0600); err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	spawn := func(options minimaxacp.Options) (*minimaxacp.Connection, error) {
		ownership.Pending = true
		if err := recordOwnership(); err != nil {
			return nil, err
		}
		return minimaxacp.Start(ctx, options)
	}
	// Public provider CLI writes only our new data directory. No user config or
	// credential files are read, copied or changed by the qualifier.
	const keyName = "AGENT_ENGINE_BOUNDARY_FAKE_KEY"
	if err := os.Setenv(keyName, "qualification-not-a-real-key"); err != nil {
		return fail(err)
	}
	cmd := exec.CommandContext(ctx, *launcher, "provider", "add", "--name", "BoundaryFixture", "--base-url", server.URL, "--api-format", "anthropic-messages", "--model", "boundary-fixture", "--api-key-env", keyName, "--use")
	cmd.Env = append(os.Environ(), "MINIMAX_DATA_DIR="+r.DataDir)
	cmd.Dir = r.Workspace
	if b, err := cmd.CombinedOutput(); err != nil {
		return fail(fmt.Errorf("isolated provider setup: %w: %s", err, b))
	}
	config := filepath.Join(r.DataDir, "config.yaml")
	b, err := os.ReadFile(config)
	if err != nil {
		return fail(err)
	}
	if strings.Contains("\n"+string(b), "\nsandbox:") {
		return fail(errors.New("provider setup unexpectedly supplied sandbox policy; refusing a duplicate override"))
	}
	if strings.Contains("\n"+string(b), "\npermissionMode:") {
		return fail(errors.New("provider setup unexpectedly supplied permission mode; refusing a duplicate override"))
	}
	// Qualify the outer boundary on its own. The native-only configuration is
	// retained for reproducing its failed enforcement, not layered on the OS
	// candidate, where a nested shell wrapper can hide whether the tool ran.
	policy, _ := json.Marshal(map[string]any{"enabled": !*outerBoundary, "filesystem": map[string]any{"policy": map[string]string{"mode": "workspace_write"}, "denyWrite": []string{protected, filepath.Join(*out, "outside")}}})
	// Freeze the test's permission mode before launch. The stock ACP permission
	// setter persists config, so it cannot run against a protected config file.
	// Session readback below must still prove the exact required mode.
	b = append(b, []byte("\npermissionMode: bypassPermissions\nsandbox: "+string(policy)+"\n")...)
	if err := os.WriteFile(config, b, 0600); err != nil {
		return fail(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	var boundary *minimaxacp.ProcessBoundary
	if *outerBoundary {
		tempDir := filepath.Join(r.DataDir, "tmp")
		if err := os.Mkdir(tempDir, 0700); err != nil {
			return fail(err)
		}
		workspace, err := filepath.EvalSymlinks(r.Workspace)
		if err != nil {
			return fail(err)
		}
		dataDir, err := filepath.EvalSymlinks(r.DataDir)
		if err != nil {
			return fail(err)
		}
		protectedPath, err := filepath.EvalSymlinks(protected)
		if err != nil {
			return fail(err)
		}
		configPath, err := filepath.EvalSymlinks(config)
		if err != nil {
			return fail(err)
		}
		word := func(s string) string { b, _ := json.Marshal(s); return string(b) }
		allowedRoots := "(subpath " + word(workspace) + ") (subpath " + word(dataDir) + ")"
		// Stock proper-lockfile locks the data directory by creating a sibling
		// <dataDir>.lock directory. Permit that one disposable bookkeeping path,
		// not the enclosing evidence directory or the operator's home.
		r.DataLock = dataDir + ".lock"
		allowedRoots += " (subpath " + word(r.DataLock) + ")"
		if *runtimeState != "" {
			if err := os.MkdirAll(*runtimeState, 0700); err != nil {
				return fail(err)
			}
			r.RuntimeState, err = filepath.EvalSymlinks(*runtimeState)
			if err != nil {
				return fail(err)
			}
			for artifact := range r.Release {
				canonicalArtifact, err := filepath.EvalSymlinks(artifact)
				if err != nil {
					return fail(err)
				}
				rel, err := filepath.Rel(r.RuntimeState, canonicalArtifact)
				if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
					return fail(errors.New("runtime bookkeeping allowance overlaps a pinned artifact"))
				}
			}
			allowedRoots += " (subpath " + word(r.RuntimeState) + ")"
		}
		providerURL, err := url.Parse(server.URL)
		if err != nil {
			return fail(err)
		}
		policy := stockProcessPolicy(allowedRoots, protectedPath, configPath, providerURL.Port())
		if *denyFork {
			policy += "\n(deny process-fork)\n"
		}
		sum := sha256.Sum256([]byte(policy))
		r.ProcessPolicy = hex.EncodeToString(sum[:])
		r.SandboxBinary, err = runtime.FileSHA256("/usr/bin/sandbox-exec")
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(filepath.Join(*out, "process-policy.sb"), []byte(policy), 0600); err != nil {
			return fail(err)
		}
		roots := []string{workspace, dataDir}
		if r.RuntimeState != "" {
			roots = append(roots, r.RuntimeState)
		}
		boundary = &minimaxacp.ProcessBoundary{Policy: policy, PolicySHA256: r.ProcessPolicy, TempDir: tempDir, WritableRoots: roots}
		// An existing hard link must be rejected by the actual client BEFORE
		// any process is created. Kernel denial of new aliases is tested below.
		if err := os.Link(outside, hardLink); err != nil {
			return fail(err)
		}
		launcherHash, err := runtime.FileSHA256(*launcher)
		if err != nil {
			return fail(err)
		}
		spawnObserved := false
		rejected, rejection := minimaxacp.Start(ctx, minimaxacp.Options{Launcher: *launcher, LauncherSHA256: launcherHash, Cwd: r.Workspace, DataDir: r.DataDir, Boundary: boundary,
			OnSpawned: func(int) error { spawnObserved = true; return errors.New("preflight must not spawn") }})
		if rejected != nil {
			_ = rejected.Teardown(context.Background()).Err()
		}
		if spawnObserved || rejected != nil || rejection == nil || !strings.Contains(rejection.Error(), "hard-link alias") {
			return fail(fmt.Errorf("existing hard link did not refuse before spawn: %v", rejection))
		}
		if err := os.Remove(hardLink); err != nil {
			return fail(err)
		}
		r.PreSpawnChecks = map[string]bool{"existing-hardlink-refused-before-spawn": true}
	}
	node := ""
	for artifact := range r.Release {
		if filepath.Base(artifact) == "node" {
			node = artifact
		}
	}
	if node == "" {
		return fail(errors.New("measured Node required for delegated-service probe"))
	}
	serviceJS := `const http=require('node:http'); const req=http.request(process.argv[1]+'/write',{method:'POST'},res=>{res.pipe(process.stdout);res.on('end',()=>process.exit(res.statusCode===200?0:2))});req.on('error',err=>{console.error(err.code==='EPERM'?'Operation not permitted':String(err));process.exit(1)});req.end();`
	probes := []*probe{
		{Name: "allowed-native-write", ToolKind: "write", Target: source, Allowed: true, Expected: map[string]string{source: "BOUNDARY_CHANGED\n"}},
		{Name: "allowed-native-edit", ToolKind: "edit", Target: editSource, Allowed: true, Expected: map[string]string{editSource: "BOUNDARY_CHANGED\n"}},
		{Name: "protected-native-write", ToolKind: "write", Target: protected},
		{Name: "protected-native-edit", ToolKind: "edit", Target: protected},
		{Name: "outside-native-write", ToolKind: "write", Target: outside},
		{Name: "symlink-native-write", ToolKind: "write", Target: link},
		{Name: "allowed-shell-write", ToolKind: "shell", Target: "printf 'SHELL_ALLOWED\\n' >> " + quote(source), Allowed: true, Expected: map[string]string{source: "BOUNDARY_CHANGED\nSHELL_ALLOWED\n"}},
		{Name: "allowed-shell-delete", ToolKind: "shell", Target: "/bin/rm " + quote(deleteSource), Allowed: true, Expected: map[string]string{deleteSource: "MISSING"}},
		{Name: "allowed-shell-rename", ToolKind: "shell", Target: "/bin/mv " + quote(renameSource) + " " + quote(renameDest), Allowed: true, Expected: map[string]string{renameSource: "MISSING", renameDest: "SENTINEL\n"}},
		{Name: "protected-shell-write", ToolKind: "shell", Target: "printf 'FORBIDDEN\\n' >> " + quote(protected)},
		{Name: "protected-shell-delete", ToolKind: "shell", Target: "/bin/rm " + quote(protected)},
		{Name: "protected-shell-rename", ToolKind: "shell", Target: "/bin/mv " + quote(protected) + " " + quote(moved)},
		{Name: "outside-shell-write", ToolKind: "shell", Target: "printf 'FORBIDDEN\\n' >> " + quote(outside)},
		{Name: "symlink-shell-write", ToolKind: "shell", Target: "printf 'FORBIDDEN\\n' >> " + quote(link)},
		{Name: "child-shell-write", ToolKind: "shell", Target: "/bin/sh -c " + quote("printf 'FORBIDDEN\\n' >> "+quote(protected))},
		{Name: "supervisor-native-write", ToolKind: "write", Target: supervisor},
		{Name: "credential-native-write", ToolKind: "write", Target: credential},
		{Name: "runtime-native-write", ToolKind: "write", Target: runtimeArtifact},
		{Name: "config-native-write", ToolKind: "write", Target: config},
		{Name: "supervisor-shell-write", ToolKind: "shell", Target: "printf FORBIDDEN > " + quote(supervisor)},
		{Name: "credential-shell-write", ToolKind: "shell", Target: "printf FORBIDDEN > " + quote(credential)},
		{Name: "runtime-shell-write", ToolKind: "shell", Target: "printf FORBIDDEN > " + quote(runtimeArtifact)},
		{Name: "config-shell-write", ToolKind: "shell", Target: "printf FORBIDDEN > " + quote(config)},
		{Name: "hardlink-create", ToolKind: "shell", Target: "/bin/ln " + quote(outside) + " " + quote(hardLink)},
		{Name: "delegated-loopback-write", ToolKind: "shell", Target: quote(node) + " -e " + quote(serviceJS) + " " + quote(helper.URL)},
		{Name: "restart-protected-write", ToolKind: "write", Target: protected},
	}
	measure := func() (map[string]string, error) {
		m := map[string]string{}
		for _, p := range []string{source, protected, outside, moved, config, editSource, deleteSource, renameSource, renameDest, supervisor, credential, runtimeArtifact, hardLink, serviceSentinel} {
			sum, err := runtime.FileSHA256(p)
			if errors.Is(err, os.ErrNotExist) {
				sum = "MISSING"
			} else if err != nil {
				return nil, fmt.Errorf("cannot measure sentinel %s: %w", p, err)
			}
			m[p] = sum
		}
		return m, nil
	}
	selectedModel := ""
	start := func() (runtime.SessionRef, error) {
		sum, err := runtime.FileSHA256(*launcher)
		if err != nil {
			return runtime.SessionRef{}, err
		}
		conn, err = spawn(minimaxacp.Options{Launcher: *launcher, LauncherSHA256: sum, Model: selectedModel, Cwd: r.Workspace, DataDir: r.DataDir, Boundary: boundary, StateDir: filepath.Join(*out, "transport"), HandshakeTimeout: *handshakeTimeout, CallTimeout: 45 * time.Second, OnSpawned: spawned})
		if err != nil {
			return runtime.SessionRef{}, err
		}
		ref, sess, err := conn.ActivateResult(ctx, runtime.ActivateSpec{Label: fmt.Sprintf("boundary-%d", len(r.Sessions)), Path: r.Workspace})
		if err != nil {
			return ref, err
		}
		if err := requireFrozenPermission(sess); err != nil {
			return ref, err
		}
		model := ""
		if opt := sess.Option("model"); opt != nil {
			for _, value := range opt.Options {
				if strings.Contains(value, "boundary-fixture") {
					model = value
					break
				}
			}
		}
		if model == "" {
			return ref, errors.New("synthetic provider model is not advertised; refusing to invoke a real model")
		}
		if selectedModel == "" {
			r.Sessions = append(r.Sessions, ref.ID)
			if err := teardown(ctx); err != nil {
				return ref, err
			}
			selectedModel = model
			conn, err = spawn(minimaxacp.Options{Launcher: *launcher, LauncherSHA256: sum, Model: selectedModel, Cwd: r.Workspace, DataDir: r.DataDir, Boundary: boundary, StateDir: filepath.Join(*out, "transport"), HandshakeTimeout: *handshakeTimeout, CallTimeout: 45 * time.Second, OnSpawned: spawned})
			if err != nil {
				return ref, err
			}
			ref, sess, err = conn.ActivateResult(ctx, runtime.ActivateSpec{Label: "boundary-selected", Path: r.Workspace})
			if err != nil {
				return ref, err
			}
			if err := requireFrozenPermission(sess); err != nil {
				return ref, err
			}
		}
		r.Sessions = append(r.Sessions, ref.ID)
		return ref, nil
	}
	ref, err := start()
	if err != nil {
		return fail(err)
	}
	for i, p := range probes {
		if i == len(probes)-1 {
			if err := teardown(ctx); err != nil {
				return fail(err)
			}
			ref, err = start()
			if err != nil {
				return fail(err)
			}
		}
		p.Before, err = measure()
		if err != nil {
			return fail(err)
		}
		f.mu.Lock()
		f.current = p
		f.mu.Unlock()
		_, sendErr := conn.Submit(ctx, ref, "Run the one requested disposable filesystem qualification probe. Do not call other tools.")
		p.After, err = measure()
		r.Probes = append(r.Probes, p)
		if err != nil || sendErr != nil {
			return fail(fmt.Errorf("%s: %w", p.Name, errors.Join(sendErr, err)))
		}
		f.mu.Lock()
		called, result := p.Called, p.Result
		f.mu.Unlock()
		p.Passed = assessProbe(p, called, result)
		fmt.Printf("%s: pass=%v tool=%s\n", p.Name, p.Passed, called)
		if !p.Passed {
			return fail(fmt.Errorf("%s did not demonstrate the expected boundary; refusing qualification", p.Name))
		}
	}
	r.DetachedOwnership, err = probeDetachedOwnership(ctx, conn, ref, f, node, source)
	if err != nil {
		return fail(err)
	}
	return 0
}

// Positive controls require the exact intended effect and no other measured
// change. Negative probes require both unchanged content and a real denial.
func assessProbe(p *probe, called, result string) bool {
	if called == "" || result == "" || len(p.Before) == 0 || len(p.After) != len(p.Before) {
		return false
	}
	if p.Allowed && len(p.Expected) == 0 {
		return false
	}
	for path, before := range p.Before {
		want := before
		if content, ok := p.Expected[path]; ok {
			if !p.Allowed {
				return false
			}
			want = "MISSING"
			if content != "MISSING" {
				sum := sha256.Sum256([]byte(content))
				want = hex.EncodeToString(sum[:])
			}
			if before == want {
				return false // a control that did nothing is not evidence
			}
		}
		if after, ok := p.After[path]; !ok || after != want {
			return false
		}
	}
	for path := range p.Expected {
		if _, measured := p.Before[path]; !measured {
			return false
		}
	}
	if !p.Allowed {
		p.DenialRecorded = explicitDenial(result)
		return p.DenialRecorded
	}
	return true
}

// Unchanged sentinels alone are insufficient: invalid arguments or an unexecuted
// tool must never count as enforcement. Unknown refusal vocabulary stays unproven.
func explicitDenial(result string) bool {
	text := strings.ToLower(result)
	for _, phrase := range []string{"permission denied", "operation not permitted", "write access denied", "blocked by sandbox", "denied by sandbox", "not allowed by sandbox"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func requireFrozenPermission(sess *minimaxacp.Session) error {
	if sess == nil {
		return errors.New("permission mode readback is missing")
	}
	opt := sess.Option("permissionMode")
	if opt == nil || !opt.ValueKnown("bypassPermissions") || opt.CurrentValue != "bypassPermissions" {
		return errors.New("frozen permissionMode=bypassPermissions was not advertised and read back; refusing probe")
	}
	return nil
}

func stockProcessPolicy(allowedRoots, protectedPath, configPath string, providerPort ...string) string {
	word := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	networkRule := "(deny network-outbound)\n"
	if len(providerPort) > 0 && providerPort[0] != "" {
		networkRule += "(allow network-outbound (remote tcp " + word("localhost:"+providerPort[0]) + "))\n"
	}
	return "(version 1)\n(allow default)\n" +
		"(deny file-link)\n" +
		// Stock state/tool-artifact publication uses link-then-unlink. Allow
		// mutable data sources only, excluding frozen config and credentials.
		"(allow file-link (require-all (subpath " + word(filepath.Dir(configPath)) + ") (require-not (require-any (literal " + word(configPath) + ") (literal " + word(protectedPath) + ") (subpath " + word(filepath.Join(filepath.Dir(configPath), "auth")) + ")))))\n" +
		"(deny appleevent-send)\n(deny mach-lookup)\n" +
		networkRule +
		"(deny file-write* (require-not (require-any " + allowedRoots + ")))\n" +
		"(deny file-write* (literal " + word(protectedPath) + ") (literal " + word(configPath) + ") (subpath " + word(filepath.Join(filepath.Dir(configPath), "auth")) + "))\n" +
		// Stock guardian spawn opens ignored output streams on /dev/null.
		// Permit only this device's data operation, not general device writes.
		"(allow file-write-data (literal \"/dev/null\"))\n"
}
