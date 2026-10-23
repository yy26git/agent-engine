package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

// episodeAPI only translates local HTTP data into existing store decisions.
// The same pinned CLI owns execution after the request has returned.
type episodeAPI struct {
	store            *control.Store
	token            string
	observerToken    string
	callerPrefix     string
	templates        map[string]bool
	start            func(string) error
	supervisorActive func(string) bool
	observers        atomic.Int32
	discoveryMu      sync.Mutex
	discoveryLatest  *episodeDiscoveryFrame
	discoverySubs    map[chan episodeDiscoveryFrame]struct{}
	discoveryCancel  context.CancelFunc
}

func (api *episodeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reject := func(status int, err error) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	}
	supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	operator := subtle.ConstantTimeCompare([]byte(supplied), []byte(api.token)) == 1
	observer := api.observerToken != "" && subtle.ConstantTimeCompare([]byte(supplied), []byte(api.observerToken)) == 1
	if !operator && !observer {
		reject(401, fmt.Errorf("local bearer authentication required"))
		return
	}
	if observer && !readOnlyEpisodeRoute(r.Method, r.URL.Path) {
		reject(403, fmt.Errorf("observer credentials are read-only"))
		return
	}
	if r.Header.Get("Origin") != "" {
		reject(403, fmt.Errorf("browser origins may not invoke local engineering control"))
		return
	}
	if r.URL.Path == "/capabilities" && r.Method == "GET" {
		row := capabilities()
		row["read_only_observer"] = api.observerToken != ""
		d, err := api.store.Deployment()
		if err == nil {
			row["deployment_id"] = d.ID
		} else if !os.IsNotExist(err) {
			reject(409, err)
			return
		} else {
			row["setup_required"] = true
		}
		_ = json.NewEncoder(w).Encode(row)
		return
	}
	if r.URL.Path == "/catalog" && r.Method == "GET" {
		c, err := api.store.Discover(api.templates)
		if err != nil {
			reject(409, err)
			return
		}
		_ = json.NewEncoder(w).Encode(c)
		return
	}
	// Discovery exposes only this endpoint's allowed templates. Neither route
	// admits work or invokes the supervisor callback.
	if r.URL.Path == "/flows" && r.Method == "GET" {
		names := []string{}
		for _, name := range api.store.Config().EpisodeTemplates.Names() {
			if len(api.templates) == 0 || api.templates[name] {
				names = append(names, name)
			}
		}
		rows, err := flowRows(api.store, names)
		if err != nil {
			reject(409, err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"flows": rows, "policy_digest": api.store.Config().Digest, "structured_output_version": 1})
		return
	}
	if r.URL.Path == "/episodes" && r.Method == "GET" {
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				reject(400, fmt.Errorf("episode limit must be between 1 and 100"))
				return
			}
			limit = parsed
		}
		rows, err := api.episodeSnapshot(limit)
		if err != nil {
			reject(500, err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"episodes": rows, "limit": limit, "read_only": observer})
		return
	}
	if r.URL.Path == "/episodes/events" && r.Method == http.MethodGet {
		api.serveDiscovery(w, r)
		return
	}
	if r.URL.Path == "/episodes/validate" && r.Method == "POST" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			reject(400, err)
			return
		}
		req, err := contract.DecodeEpisodeRequest(body)
		if err != nil {
			reject(400, err)
			return
		}
		if !strings.HasPrefix(req.Origin.CallerID, api.callerPrefix) || !strings.HasPrefix(req.EpisodeID, api.callerPrefix) {
			reject(403, fmt.Errorf("request is outside this caller's approved namespace"))
			return
		}
		if len(api.templates) > 0 && !api.templates[req.Template] {
			reject(403, fmt.Errorf("template is outside this caller's locally approved workflows"))
			return
		}
		// Share the CLI's preflight and machine-readable response rather than creating
		// another validator. No request files are created by an API preview.
		if err := episodePreflight(api.store, req); err != nil {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(prerequisiteRefusal(err))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ready": true, "episode_id": req.EpisodeID,
			"agents_started": false, "live_provider_proven": false, "policy_digest": api.store.Config().Digest,
			"next_action": "submit the exact request to POST /episodes; admission rechecks policy"})
		return
	}
	if r.URL.Path == "/readiness" && r.Method == "GET" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "scope": "local-api", "policy_digest": api.store.Config().Digest, "live_acceptance_proven": false})
		return
	}
	if r.URL.Path == "/repair-bundles" && r.Method == "POST" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			reject(400, err)
			return
		}
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		var req control.RepairBundleRequest
		if err := dec.Decode(&req); err != nil {
			reject(400, err)
			return
		}
		var trailing any
		if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
			reject(400, fmt.Errorf("repair bundle request must contain exactly one JSON value"))
			return
		}
		if !strings.HasPrefix(req.EpisodeID, api.callerPrefix) {
			reject(403, fmt.Errorf("episode is outside this caller's approved namespace"))
			return
		}
		if len(api.templates) > 0 && !api.templates[req.Template] {
			reject(403, fmt.Errorf("template is outside this caller's locally approved workflows"))
			return
		}
		receipt, err := api.store.PrepareRepairBundle(req)
		if err != nil {
			reject(409, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(receipt)
		return
	}
	if r.URL.Path == "/episodes" && r.Method == "POST" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			reject(400, err)
			return
		}
		req, err := contract.DecodeEpisodeRequest(body)
		if err != nil {
			reject(400, err)
			return
		}
		if !strings.HasPrefix(req.Origin.CallerID, api.callerPrefix) {
			reject(403, fmt.Errorf("caller is outside the locally approved namespace"))
			return
		}
		if len(api.templates) > 0 && !api.templates[req.Template] {
			reject(403, fmt.Errorf("template is outside this caller's locally approved workflows"))
			return
		}
		adm, err := api.store.AdmitEpisode(req)
		if err != nil {
			reject(409, err)
			return
		}
		if err := api.start(req.EpisodeID); err != nil {
			reject(503, fmt.Errorf("episode admitted but supervision could not start: %w", err))
			return
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "episode_id": req.EpisodeID, "request_id": adm.RequestID, "task_id": adm.TaskID, "stage": adm.Stage})
		return
	}
	pieces := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pieces) < 2 || len(pieces) > 3 || pieces[0] != "episodes" {
		reject(404, fmt.Errorf("unknown route"))
		return
	}
	id := pieces[1]
	request, err := api.store.Request(id)
	if err != nil || !strings.HasPrefix(request.Origin.CallerID, api.callerPrefix) {
		// Namespace checks apply to reading, resuming and cancellation too.
		// Do not reveal another caller's episode or launch its supervisor.
		reject(404, fmt.Errorf("unknown episode"))
		return
	}
	st, err := api.store.State(id)
	if err != nil || st.Episode == nil {
		reject(404, fmt.Errorf("unknown episode"))
		return
	}
	if len(api.templates) > 0 && !api.templates[st.EpisodeTemplate] {
		reject(404, fmt.Errorf("unknown episode"))
		return
	}
	if len(pieces) == 3 && pieces[2] == "events" && r.Method == "GET" {
		api.serveProgress(w, r, id)
		return
	}
	if len(pieces) == 2 && r.Method == "GET" {
		if !observer && !st.Stage.Terminal() {
			if err := api.start(id); err != nil {
				reject(503, err)
				return
			}
		}
		health := api.executionHealth(st)
		row := map[string]any{"episode_id": id, "request_id": st.RequestID, "stage": st.Stage, "phase": st.Episode.Phase, "stage_reason": st.StageReason, "revision": st.Revision, "terminal": st.Stage.Terminal(), "execution_health": health.health, "requires_action": health.requiresAction, "next_action": health.nextAction, "plan": st.Episode.PlanDigest, "authorized": st.Episode.PlanAuthorized}
		if health.supervisorExit != "" {
			row["supervisor_exit_classification"] = health.supervisorExit
		}
		if observer {
			// Observer responses are a deliberately smaller projection. Pinned
			// ProfileRefs contain launch argv and environment values, which must
			// never cross the read-only presentation boundary.
			row["execution_profiles"] = publicExecutionProfiles(st.ExecutionProfiles)
		} else {
			row["workspace"] = st.Workspace
			row["execution_profiles"] = st.ExecutionProfiles
			row["execution_assurance"] = st.ExecutionAssurance
			row["repair_branch"] = st.RepairBranch
			row["repair_base_head"] = st.RepairBaseHead
			row["repair_bundle_sha256"] = st.RepairBundleSHA256
			row["repair_bundle_targets"] = st.RepairBundleTargets
		}
		_ = json.NewEncoder(w).Encode(row)
		return
	}
	if len(pieces) == 3 && pieces[2] == "output" && r.Method == "GET" {
		output, err := api.store.ReportOutput(id)
		if err != nil {
			reject(409, err)
			return
		}
		_ = json.NewEncoder(w).Encode(output)
		return
	}
	if len(pieces) == 3 && pieces[2] == "result" && r.Method == "GET" {
		res, err := api.store.Result(id)
		if err != nil {
			reject(409, err)
			return
		}
		if res == nil || !res.Ready {
			_ = json.NewEncoder(w).Encode(map[string]bool{"ready": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "status": res.Status, "summary": res.Summary, "reason": res.Reason, "result": res})
		return
	}
	if len(pieces) == 3 && pieces[2] == "cancel" && r.Method == "POST" {
		_, err := api.store.UpdateCommitted(id, func(s *control.State) error {
			if !s.Stage.Terminal() {
				s.CancellationRequested = true
				s.CancellationReason = "authenticated local API cancellation"
				s.StageReason = "cancellation requested; effects pending reconciliation"
			}
			return nil
		})
		if err != nil {
			reject(409, err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"requested": true})
		return
	}
	reject(405, fmt.Errorf("method or operation unsupported"))
}

func cmdServe(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("serve", stderr)
	address := fs.String("listen", "127.0.0.1:8787", "literal loopback address")
	tokenFile := fs.String("token-file", "", "operator-owned local bearer token file (0600)")
	observerTokenFile := fs.String("observer-token-file", "", "optional read-only observer token file (0600)")
	prefix := fs.String("caller-prefix", "", "approved caller namespace; required")
	templateNames := fs.String("templates", "", "comma-separated local workflow allowlist; empty permits all registered templates")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fail(stdout, stderr, asJSON, fmt.Errorf("serve requires a literal loopback address"))
	}
	if *prefix == "" || !filepath.IsAbs(*tokenFile) {
		return fail(stdout, stderr, asJSON, fmt.Errorf("--caller-prefix and an absolute --token-file are required"))
	}
	canonicalToken, err := filepath.EvalSymlinks(*tokenFile)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	for _, root := range st.Config().SourceRoots {
		if rel, err := filepath.Rel(root, canonicalToken); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fail(stdout, stderr, asJSON, fmt.Errorf("token file must be outside approved source roots"))
		}
	}
	info, err := os.Stat(*tokenFile)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fail(stdout, stderr, asJSON, fmt.Errorf("token file must be a private regular file"))
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 {
		return fail(stdout, stderr, asJSON, fmt.Errorf("bearer token must contain at least 32 bytes"))
	}
	observerToken := ""
	if *observerTokenFile != "" {
		observerToken, err = readPrivateServiceToken(*observerTokenFile, st.Config().SourceRoots)
		if err != nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf("observer token: %w", err))
		}
		if len(observerToken) < 32 || subtle.ConstantTimeCompare([]byte(observerToken), []byte(token)) == 1 {
			return fail(stdout, stderr, asJSON, fmt.Errorf("observer token must contain at least 32 bytes and differ from the operator token"))
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var mu sync.Mutex
	running := map[string]*exec.Cmd{}
	var workers sync.WaitGroup
	templates := map[string]bool{}
	if *templateNames != "" {
		for _, name := range strings.Split(*templateNames, ",") {
			name = strings.TrimSpace(name)
			if _, ok := st.Config().EpisodeTemplates.Lookup(name); !ok {
				return fail(stdout, stderr, asJSON, fmt.Errorf("unapproved caller workflow %q", name))
			}
			templates[name] = true
		}
	}
	var profiles []control.ProfileRef
	for _, name := range st.Config().EpisodeTemplates.Names() {
		if len(templates) > 0 && !templates[name] {
			continue
		}
		template, _ := st.Config().EpisodeTemplates.Lookup(name)
		for _, profileName := range template.Roles {
			profiles = append(profiles, st.Config().Profiles[profileName].Ref())
		}
	}
	if err := requireCodexRuntimeHome(profiles); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	api := &episodeAPI{store: st, token: token, observerToken: observerToken, callerPrefix: *prefix, templates: templates}
	api.supervisorActive = func(id string) bool {
		mu.Lock()
		defer mu.Unlock()
		_, ok := running[id]
		return ok
	}
	api.start = func(id string) error {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := running[id]; ok {
			return nil
		}
		state, err := st.State(id)
		if err != nil {
			return err
		}
		if state.Stage.Terminal() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A previous service may have left its supervisor alive. The request
		// ownership lock, rather than this service's in-memory map, is authority.
		release, err := st.AcquireOwnership(id)
		if err != nil {
			if errors.Is(err, control.ErrOwnershipHeld) {
				return nil
			}
			return err
		}
		release()

		dir, err := st.RequestDir(id)
		if err != nil {
			return err
		}
		log, err := os.OpenFile(filepath.Join(dir, "supervisor.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		cmd := exec.Command(executable, "--store", st.Config().StoreRoot, "run", "--request-id", id)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "AGENT_ENGINE_LOCAL_TOKEN=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			return err
		}
		running[id] = cmd
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := cmd.Wait()
			log.Close()
			mu.Lock()
			delete(running, id)
			mu.Unlock()
			if err != nil {
				fmt.Fprintf(stderr, "episode %s supervisor stopped: %v\n", id, err)
			}
		}()
		return nil
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	server := &http.Server{Handler: api, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintf(stdout, "Agent Engine local API listening at http://%s\n", listener.Addr())
	intentionalServiceStop := false
	select {
	case <-ctx.Done():
		intentionalServiceStop = true
	case err = <-done:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	mu.Lock()
	type ownedSupervisor struct {
		id  string
		cmd *exec.Cmd
	}
	owned := make([]ownedSupervisor, 0, len(running))
	for id, cmd := range running {
		owned = append(owned, ownedSupervisor{id: id, cmd: cmd})
	}
	mu.Unlock()
	for _, current := range owned {
		if current.cmd.Process == nil {
			continue
		}
		if intentionalServiceStop {
			pid := current.cmd.Process.Pid
			if err := st.RequestSupervisorStop(current.id, pid, control.StartTokenFor(pid), "service shutdown", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				fmt.Fprintf(stderr, "episode %s: could not persist supervisor stop intent: %v\n", current.id, err)
			}
		}
		_ = current.cmd.Process.Signal(syscall.SIGTERM)
	}
	joined := make(chan struct{})
	go func() { workers.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(15 * time.Second):
		return fail(stdout, stderr, asJSON, fmt.Errorf("supervisors did not stop; ownership recovery is required before replacement"))
	}
	if err != nil && err != http.ErrServerClosed {
		return fail(stdout, stderr, asJSON, err)
	}
	return 0
}

func readOnlyEpisodeRoute(method, requestPath string) bool {
	if method != http.MethodGet {
		return false
	}
	switch requestPath {
	case "/capabilities", "/catalog", "/flows", "/readiness", "/episodes", "/episodes/events":
		return true
	}
	parts := strings.Split(strings.Trim(requestPath, "/"), "/")
	if len(parts) == 2 && parts[0] == "episodes" {
		return true
	}
	return len(parts) == 3 && parts[0] == "episodes" && (parts[2] == "events" || parts[2] == "result" || parts[2] == "output")
}

func readPrivateServiceToken(filename string, sourceRoots []string) (string, error) {
	if !filepath.IsAbs(filename) {
		return "", fmt.Errorf("token path must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return "", err
	}
	for _, root := range sourceRoots {
		if rel, err := filepath.Rel(root, canonical); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("token file must be outside approved source roots")
		}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("token file must be a private regular file")
	}
	raw, err := os.ReadFile(canonical)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
