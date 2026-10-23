package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/runtime"
)

// Qualification uses the same Agent Deck create/start/output/stop seam and exact
// approved Codex globals, substituting a deterministic sandbox command for a
// model turn. Nothing in qualification authorizes an engineering episode.
func cmdQualify(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("qualify", stderr)
	name := fs.String("profile", "", "read-only Codex profile")
	workspace := fs.String("workspace", "", "approved disposable workspace")
	out := fs.String("out", "", "absolute receipt path in operator store")
	timeout := fs.Duration("timeout", 90*time.Second, "overall qualification deadline")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *timeout <= 0 || *timeout > 5*time.Minute {
		return fail(stdout, stderr, asJSON, fmt.Errorf("qualification timeout must be positive and at most 5m"))
	}
	p, ok := st.Config().Profiles[*name]
	if !ok {
		return fail(stdout, stderr, asJSON, fmt.Errorf("unknown qualification profile"))
	}
	if !filepath.IsAbs(*out) || !filepath.IsAbs(*workspace) {
		return fail(stdout, stderr, asJSON, fmt.Errorf("--workspace and --out must be absolute"))
	}
	canonical, err := filepath.EvalSymlinks(filepath.Dir(*out))
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	rel, err := filepath.Rel(st.Config().StoreRoot, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fail(stdout, stderr, asJSON, fmt.Errorf("receipt must be inside operator store"))
	}
	if _, ok := st.Config().ApprovedSourceRoot(canonical); ok {
		return fail(stdout, stderr, asJSON, fmt.Errorf("receipt cannot be under an approved source root"))
	}
	// Do not overwrite an existing approval or failed attempt. Each qualification
	// gets a new path; a profile can explicitly select a successfully retained one.
	lock, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	lock.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	r := control.ConfinementReceipt{Schema: 1, Status: "RUNNING", Profile: p.Name, Workspace: *workspace, Mechanism: string(control.CodexReadOnly), StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	retain := func() error { return retainQualification(*out, r) }
	if err := retain(); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	identity, err := st.Config().ConfinementIdentity(p, *workspace)
	if err == nil {
		r.Identity = identity
		var rt *agentdeck.CLIRuntime
		rt, err = pinnedRuntime(st.Config(), stderr, false)
		if err == nil {
			err = runConfinementQualification(ctx, rt, p, *workspace, &r, retain, filepath.Dir(*out))
		}
	}
	r.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		r.Status = "FAILED"
		r.Failure = err.Error()
	} else {
		r.Status = "PASSED"
	}
	if saveErr := retain(); saveErr != nil {
		return fail(stdout, stderr, asJSON, saveErr)
	}
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(r)
	} else {
		fmt.Fprintf(stdout, "Qualification %s: %s\nReceipt: %s\n", p.Name, r.Status, *out)
		if r.Failure != "" {
			fmt.Fprintln(stderr, r.Failure)
		}
	}
	if err != nil {
		return 2
	}
	return 0
}

func retainQualification(path string, r control.ConfinementReceipt) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".qualification-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(body, '\n')); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func qualifyWord(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func runConfinementQualification(ctx context.Context, rt agentdeck.Runtime, p control.Profile, workspace string, r *control.ConfinementReceipt, retain func() error, logDirs ...string) (retErr error) {
	args, err := control.CodexQualificationCommand(p)
	if err != nil {
		return err
	}
	probeDir, err := os.MkdirTemp(workspace, ".emjin-qualification-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probeDir)
	outside, err := os.MkdirTemp("", "emjin-qualification-outside-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(outside)
	source, protected, external := filepath.Join(probeDir, "source"), filepath.Join(probeDir, "protected"), filepath.Join(outside, "sentinel")
	for _, path := range []string{source, protected, external} {
		if err = os.WriteFile(path, []byte("qualification sentinel\n"), 0600); err != nil {
			return err
		}
	}
	link := filepath.Join(probeDir, "escape")
	if err = os.Symlink(external, link); err != nil {
		return err
	}
	probes := []struct {
		kind, path string
		read       bool
	}{{"read-control", source, true}, {"source-write", source, false}, {"protected-write", protected, false}, {"outside-write", external, false}, {"symlink-write", link, false}}
	var sessions []string
	unknownCreate := false
	defer func() {
		clean := !unknownCreate
		var cleanupErrors []error
		for _, id := range sessions {
			// Each of the at most five owned probes gets a bounded stop window.
			// Sharing one deadline made later probes inherit earlier cleanup time.
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := rt.Stop(cleanup, id); err != nil {
				clean = false
				cleanupErrors = append(cleanupErrors, fmt.Errorf("stop %s: %w", id, err))
				cancel()
				continue
			}
			for {
				s, err := rt.Inspect(cleanup, id)
				if err == nil && s.Status == "stopped" {
					if remover, ok := rt.(interface {
						RemoveRetired(context.Context, string) error
					}); ok {
						if err := remover.RemoveRetired(cleanup, id); err != nil {
							clean = false
							cleanupErrors = append(cleanupErrors, fmt.Errorf("remove retired %s: %w", id, err))
						}
					}
					break
				}
				if cleanup.Err() != nil {
					clean = false
					cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect stopped %s: %w", id, cleanup.Err()))
					break
				}
				select {
				case <-cleanup.Done():
					clean = false
				case <-time.After(100 * time.Millisecond):
				}
			}
			cancel()
		}
		r.CleanupProven = clean
		for i := range r.Probes {
			r.Probes[i].Clean = clean
		}
		if !clean {
			retErr = errors.Join(retErr, fmt.Errorf("qualification process cleanup or retired-session removal is unproven"), errors.Join(cleanupErrors...))
		}
		if err := retain(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	for _, probe := range probes {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := runtime.FileSHA256(probe.path)
		if err != nil {
			return err
		}
		nonce := fmt.Sprintf("EXQUAL_%d_%d", os.Getpid(), time.Now().UnixNano())
		label := nonce + "_" + probe.kind
		// Markers are printed by the launch wrapper, never by a model. A denied
		// write alone is insufficient: the read-control must first prove startup.
		script := "printf changed > \"$1\""
		if probe.read {
			script = "cat \"$1\""
		}
		var words []string
		if len(p.LaunchEnv) > 0 {
			words = append(words, "env")
			for key, value := range p.LaunchEnv {
				words = append(words, qualifyWord(key+"="+value))
			}
		}
		for _, arg := range append(args, "sandbox", "/bin/sh", "-c", script, "qualification", probe.path) {
			words = append(words, qualifyWord(arg))
		}
		logPath := ""
		if len(logDirs) > 0 {
			log, err := os.CreateTemp(logDirs[0], ".qualification-output-")
			if err != nil {
				return err
			}
			logPath = log.Name()
			if err := log.Close(); err != nil {
				return err
			}
		}
		command := qualificationWrapper(strings.Join(words, " "), nonce, logPath)
		r.Probes = append(r.Probes, control.ConfinementProbe{Kind: probe.kind, Label: label, Marker: nonce, Command: command, OutputPath: logPath, Before: before, ExitCode: -1})
		index := len(r.Probes) - 1
		if err := retain(); err != nil {
			return err
		}
		spec, err := p.CreateSpec(workspace, label)
		if err != nil {
			return err
		}
		spec.Wrapper = command
		s, err := rt.Create(ctx, spec)
		if err != nil {
			unknownCreate = true
			return fmt.Errorf("qualification create outcome is unknown; reconcile label %s: %w", label, err)
		}
		sessions = append(sessions, s.ID)
		r.Probes[index].SessionID = s.ID
		if err := retain(); err != nil {
			return err
		}
		if err := rt.Start(ctx, s.ID); err != nil {
			return err
		}
		for {
			output := agentdeck.Output{SessionID: s.ID}
			if logPath != "" {
				body, err := os.ReadFile(logPath)
				if err != nil {
					return err
				}
				if len(body) > 65536 {
					return fmt.Errorf("qualification output exceeded 64KiB")
				}
				output.Text = string(body)
			} else {
				output, err = rt.Output(ctx, s.ID, 4096)
				if err != nil {
					return err
				}
			}
			if output.SessionID != s.ID || output.Truncated {
				return fmt.Errorf("qualification output identity is mismatched or truncated")
			}
			if at := strings.LastIndex(output.Text, nonce+":"); at >= 0 {
				value := strings.Fields(output.Text[at+len(nonce)+1:])
				if len(value) == 0 {
					return fmt.Errorf("empty qualification marker")
				}
				code, err := strconv.Atoi(value[0])
				if err != nil {
					return err
				}
				after, err := runtime.FileSHA256(probe.path)
				if err != nil {
					return err
				}
				row := &r.Probes[index]
				row.Output = output.Text
				if logPath != "" {
					row.OutputSHA256, err = runtime.FileSHA256(logPath)
					if err != nil {
						return err
					}
				}
				row.ExitCode = code
				row.After = after
				row.Complete = true
				if err := retain(); err != nil {
					return err
				}
				if after != before {
					return fmt.Errorf("forbidden write succeeded in %s", probe.kind)
				}
				if probe.read && code != 0 {
					return fmt.Errorf("Codex sandbox read-control could not start: %s", output.Text)
				}
				if !probe.read && (code == 0 || (!strings.Contains(output.Text, "Operation not permitted") && !strings.Contains(output.Text, "Permission denied") && !strings.Contains(output.Text, "Read-only file system"))) {
					return fmt.Errorf("%s did not demonstrate a sandbox denial: %s", probe.kind, output.Text)
				}
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil
}

// The bounded wrapper stays alive for collection; Stop joins its retirement.
// Output is retained outside the source tree even if tmux loses its history.
func qualificationWrapper(invocation, nonce, logPath string) string {
	if logPath == "" {
		return invocation + `; result=$?; printf '\n` + nonce + `:%s\n' "$result"; exec /bin/sleep 300`
	}
	log := qualifyWord(logPath)
	return invocation + " > " + log + ` 2>&1; result=$?; printf '\n` + nonce + `:%s\n' "$result" >> ` + log + "; cat " + log + "; exec /bin/sleep 300"
}
