package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	hudBundleName = "AgentEngineHUD.app"
	hudExecutable = "AgentEngineHUD"
	hudBundleID   = "com.emjin.agent-engine.hud"
)

type hudBundle struct {
	path       string
	executable string
}

type hudProcess struct {
	PID  int    `json:"pid"`
	Path string `json:"app_path"`
}

// cmdHUD is an optional UI lifecycle command. It intentionally has no access
// to a Store: starting or stopping the viewer cannot start, stop, or mutate an
// episode or the Engine service.
func cmdHUD(args []string, stdout, stderr io.Writer, asJSON bool) int {
	if runtime.GOOS != "darwin" {
		return fail(stdout, stderr, asJSON, errors.New("the native floating HUD companion is macOS-only; the rest of the Agent Engine CLI remains available on this platform"))
	}
	fs := flagSet("hud", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agent-engine hud <start|status|stop>")
		return 2
	}
	action := fs.Arg(0)
	if action != "start" && action != "status" && action != "stop" {
		return fail(stdout, stderr, asJSON, fmt.Errorf("unknown HUD action %q (expected start, status or stop)", action))
	}

	bundles, err := installedHUDBundles()
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if len(bundles) == 0 {
		if action == "status" {
			result := map[string]any{"ok": true, "available": false, "running": false, "platform": runtime.GOOS, "message": "AgentEngineHUD.app is not installed; build it with scripts/build-agent-engine-hud.sh and place it in ~/Applications or /Applications"}
			if asJSON {
				return emit(stdout, stderr, result)
			}
			fmt.Fprintln(stdout, "Agent Engine HUD is not installed. Build it with scripts/build-agent-engine-hud.sh and place AgentEngineHUD.app in ~/Applications or /Applications.")
			return 0
		}
		return fail(stdout, stderr, asJSON, errors.New("AgentEngineHUD.app is not installed; build it with scripts/build-agent-engine-hud.sh and place it in ~/Applications or /Applications"))
	}

	switch action {
	case "status":
		processes, err := findHUDProcesses(bundles)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		return hudResult(stdout, stderr, asJSON, map[string]any{
			"ok": true, "available": true, "running": len(processes) > 0,
			"processes": processes, "app_path": bundles[0].path,
		})
	case "start":
		processes, err := findHUDProcesses(bundles)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		if len(processes) > 0 {
			return hudResult(stdout, stderr, asJSON, map[string]any{
				"ok": true, "available": true, "running": true,
				"action": "already_running", "processes": processes,
			})
		}
		// Launch Services opens only the fixed, prevalidated app bundle. Event or
		// response data can never nominate an executable path or arguments.
		cmd := exec.Command("/usr/bin/open", "-a", bundles[0].path)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf("opening AgentEngineHUD.app: %w: %s", err, strings.TrimSpace(string(output))))
		}
		confirmed, _ := waitForHUDState(bundles, true, 3*time.Second)
		if len(confirmed) > 0 {
			return hudResult(stdout, stderr, asJSON, map[string]any{
				"ok": true, "available": true, "running": true,
				"action": "started", "processes": confirmed,
			})
		}
		return hudResult(stdout, stderr, asJSON, map[string]any{
			"ok": true, "available": true, "running": false,
			"action": "launch_requested", "app_path": bundles[0].path,
			"message": "Launch Services accepted the request; the HUD process was not yet observable",
		})
	case "stop":
		processes, err := findHUDProcesses(bundles)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		if len(processes) == 0 {
			return hudResult(stdout, stderr, asJSON, map[string]any{
				"ok": true, "available": true, "running": false, "action": "already_stopped",
			})
		}
		for _, p := range processes {
			bundle, ok := bundleForExecutable(bundles, p.Path)
			if !ok {
				continue
			}
			// Recheck the executable immediately before signaling. If the PID was
			// reused, the current process will fail this exact-path check.
			currentPath, alive, err := processExecutable(p.PID)
			if err != nil {
				return fail(stdout, stderr, asJSON, err)
			}
			if !alive || currentPath != bundle.executable {
				continue
			}
			kill := exec.Command("/bin/kill", "-TERM", strconv.Itoa(p.PID))
			if output, err := kill.CombinedOutput(); err != nil {
				return fail(stdout, stderr, asJSON, fmt.Errorf("stopping AgentEngineHUD pid %d: %w: %s", p.PID, err, strings.TrimSpace(string(output))))
			}
		}
		remaining, err := waitForHUDState(bundles, false, 5*time.Second)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		if len(remaining) != 0 {
			return fail(stdout, stderr, asJSON, fmt.Errorf("AgentEngineHUD did not stop within 5 seconds; remaining verified process count: %d", len(remaining)))
		}
		return hudResult(stdout, stderr, asJSON, map[string]any{
			"ok": true, "available": true, "running": false, "action": "stopped",
		})
	}
	return 2
}

func hudResult(stdout, stderr io.Writer, asJSON bool, result map[string]any) int {
	if asJSON {
		return emit(stdout, stderr, result)
	}
	if running, _ := result["running"].(bool); running {
		if action, _ := result["action"].(string); action != "" {
			fmt.Fprintf(stdout, "Agent Engine HUD: %s\n", strings.ReplaceAll(action, "_", " "))
		} else {
			fmt.Fprintln(stdout, "Agent Engine HUD is running.")
		}
		return 0
	}
	if action, _ := result["action"].(string); action != "" {
		fmt.Fprintf(stdout, "Agent Engine HUD: %s\n", strings.ReplaceAll(action, "_", " "))
	} else if message, _ := result["message"].(string); message != "" {
		fmt.Fprintln(stdout, message)
	} else {
		fmt.Fprintln(stdout, "Agent Engine HUD is not running.")
	}
	return 0
}

func installedHUDBundles() ([]hudBundle, error) {
	candidates := hudBundleCandidates()
	seen := map[string]bool{}
	var bundles []hudBundle
	var invalid []string
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		bundle, exists, err := validateHUDBundle(candidate)
		if err != nil {
			invalid = append(invalid, err.Error())
			continue
		}
		if exists {
			bundles = append(bundles, bundle)
		}
	}
	if len(bundles) == 0 && len(invalid) > 0 {
		return nil, fmt.Errorf("no trusted AgentEngineHUD.app found: %s", strings.Join(invalid, "; "))
	}
	return bundles, nil
}

func hudBundleCandidates() []string {
	var candidates []string
	addDistAncestors := func(start string) {
		if start == "" {
			return
		}
		current, err := filepath.Abs(start)
		if err != nil {
			return
		}
		for depth := 0; depth < 12; depth++ {
			candidates = append(candidates, filepath.Join(current, "dist", hudBundleName))
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		addDistAncestors(cwd)
	}
	if executable, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		addDistAncestors(filepath.Dir(executable))
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		candidates = append(candidates, filepath.Join(home, "Applications", hudBundleName))
	}
	candidates = append(candidates, filepath.Join(string(filepath.Separator), "Applications", hudBundleName))
	return candidates
}

var plistValuePattern = func() *regexp.Regexp {
	return regexp.MustCompile(`(?s)<key>%s</key>\s*<string>([^<]*)</string>`)
}()

func validateHUDBundle(path string) (hudBundle, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return hudBundle{}, false, nil
	}
	if err != nil {
		return hudBundle{}, false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return hudBundle{}, false, fmt.Errorf("refusing unsafe HUD bundle path %s", path)
	}
	contents := filepath.Join(path, "Contents")
	macOSDir := filepath.Join(contents, "MacOS")
	for _, directory := range []string{contents, macOSDir} {
		d, err := os.Lstat(directory)
		if err != nil || !d.IsDir() || d.Mode()&os.ModeSymlink != 0 || d.Mode().Perm()&0022 != 0 {
			return hudBundle{}, false, fmt.Errorf("refusing unsafe HUD bundle directory %s", directory)
		}
	}
	plistPath := filepath.Join(contents, "Info.plist")
	plistInfo, err := os.Lstat(plistPath)
	if err != nil || !plistInfo.Mode().IsRegular() || plistInfo.Mode()&os.ModeSymlink != 0 || plistInfo.Mode().Perm()&0022 != 0 {
		return hudBundle{}, false, fmt.Errorf("refusing missing or unsafe HUD metadata %s", plistPath)
	}
	if plistInfo.Size() > 1024*1024 {
		return hudBundle{}, false, fmt.Errorf("refusing oversized HUD metadata %s", plistPath)
	}
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		return hudBundle{}, false, fmt.Errorf("read HUD metadata %s: %w", plistPath, err)
	}
	value := func(key string) string {
		pattern := regexp.MustCompile(fmt.Sprintf(plistValuePattern.String(), regexp.QuoteMeta(key)))
		matches := pattern.FindAllSubmatch(plist, -1)
		if len(matches) != 1 {
			return ""
		}
		return string(matches[0][1])
	}
	if value("CFBundleIdentifier") != hudBundleID || value("CFBundleExecutable") != hudExecutable {
		return hudBundle{}, false, fmt.Errorf("refusing HUD bundle with unexpected identity at %s", path)
	}
	executable := filepath.Join(macOSDir, hudExecutable)
	exeInfo, err := os.Lstat(executable)
	if err != nil || !exeInfo.Mode().IsRegular() || exeInfo.Mode()&os.ModeSymlink != 0 || exeInfo.Mode().Perm()&0111 == 0 || exeInfo.Mode().Perm()&0022 != 0 {
		return hudBundle{}, false, fmt.Errorf("refusing missing or unsafe HUD executable %s", executable)
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return hudBundle{}, false, fmt.Errorf("resolve HUD executable %s: %w", executable, err)
	}
	return hudBundle{path: path, executable: resolvedExecutable}, true, nil
}

func findHUDProcesses(bundles []hudBundle) ([]hudProcess, error) {
	output, err := exec.Command("/usr/bin/pgrep", "-x", hudExecutable).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("locating HUD processes: %w", err)
	}
	var processes []hudProcess
	for _, line := range strings.Fields(string(output)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid < 1 {
			continue
		}
		path, alive, err := processExecutable(pid)
		if err != nil {
			return nil, err
		}
		if !alive {
			continue
		}
		for _, bundle := range bundles {
			if path == bundle.executable {
				processes = append(processes, hudProcess{PID: pid, Path: bundle.path})
				break
			}
		}
	}
	return processes, nil
}

func processExecutable(pid int) (string, bool, error) {
	output, err := exec.Command("/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fn").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("cannot verify executable identity for pid %d: %w", pid, err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "n/") {
			path := strings.TrimPrefix(line, "n")
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				path = resolved
			}
			return path, true, nil
		}
	}
	return "", false, nil
}

func bundleForExecutable(bundles []hudBundle, appPath string) (hudBundle, bool) {
	for _, bundle := range bundles {
		if bundle.path == appPath {
			return bundle, true
		}
	}
	return hudBundle{}, false
}

func waitForHUDState(bundles []hudBundle, running bool, timeout time.Duration) ([]hudProcess, error) {
	deadline := time.Now().Add(timeout)
	for {
		processes, err := findHUDProcesses(bundles)
		if err == nil && (len(processes) > 0) == running {
			return processes, nil
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return nil, fmt.Errorf("checking HUD process state: %w", err)
			}
			return processes, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
}
