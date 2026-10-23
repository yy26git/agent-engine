// Package cursorcli owns bounded headless Cursor report execution. It has no
// admission, review or engineering-result authority.
package cursorcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Options struct {
	Executable, Model, Workspace, Prompt string
	OnSpawned                            func(int) error
}
type Turn struct{ Text, SessionID, ReportedModel string }
type boundedBuffer struct {
	mu       sync.Mutex
	b        bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := b.limit - b.b.Len()
	if len(p) > room {
		p = p[:room]
		b.overflow = true
	}
	b.b.Write(p)
	return n, nil
}
func Run(ctx context.Context, o Options) (Turn, error) {
	if o.OnSpawned == nil {
		return Turn{}, errors.New("Cursor execution requires a durable spawn callback")
	}
	// stdin avoids putting Forge's log/evidence content into the process argv.
	cmd := exec.CommandContext(ctx, o.Executable, "--print", "--mode", "ask", "--model", o.Model, "--output-format", "json", "--trust", "--workspace", o.Workspace)
	cmd.Dir = o.Workspace
	cmd.Stdin = strings.NewReader(o.Prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Group cancellation is bounded and reaches children of the CLI launcher.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	out := &boundedBuffer{limit: 3 * 1024 * 1024}
	stderr := &boundedBuffer{limit: 8192}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return Turn{}, err
	}
	if err := o.OnSpawned(cmd.Process.Pid); err != nil {
		killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(killErr, syscall.ESRCH) {
			killErr = nil
		}
		waitErr := cmd.Wait()
		return Turn{}, errors.Join(err, killErr, waitErr, quiesceGroup(cmd.Process.Pid))
	}
	err := cmd.Wait()
	// Descendants may outlive a successful launcher. Never leave them running.
	groupErr := quiesceGroup(cmd.Process.Pid)
	if groupErr != nil {
		return Turn{}, fmt.Errorf("Cursor group cleanup: %w", groupErr)
	}
	if err != nil {
		return Turn{}, fmt.Errorf("Cursor report process failed: %w; %s", err, stderr.b.String())
	}
	if out.overflow {
		return Turn{}, errors.New("Cursor response exceeds transport limit")
	}
	var env struct {
		Type, Subtype, Result string
		IsError               bool   `json:"is_error"`
		SessionID             string `json:"session_id"`
		Model                 string `json:"model"`
	}
	if err := json.Unmarshal(out.b.Bytes(), &env); err != nil {
		return Turn{}, fmt.Errorf("Cursor response envelope: %w", err)
	}
	if env.Type != "result" || env.Subtype != "success" || env.IsError || env.SessionID == "" {
		return Turn{}, errors.New("Cursor did not return a successful identified result")
	}
	text := strings.TrimSpace(env.Result)
	if strings.HasPrefix(text, "```json\n") && strings.HasSuffix(text, "\n```") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json\n"), "\n```"))
	}
	return Turn{Text: text, SessionID: env.SessionID, ReportedModel: env.Model}, nil
}

// The child is its own process-group leader. Completion is not published until
// the launcher has been waited and its process group is observed absent.
func quiesceGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
		return nil
	} else if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("Cursor process group still exists after cleanup")
}
