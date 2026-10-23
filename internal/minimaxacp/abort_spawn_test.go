package minimaxacp

import (
	"errors"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func abortFixture(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return cmd
}

func TestAbortSpawn_UnknownProbeCannotEstablishQuiescence(t *testing.T) {
	for _, probeErr := range []error{syscall.EPERM, syscall.EIO} {
		t.Run(probeErr.Error(), func(t *testing.T) {
			cmd := abortFixture(t)
			err := abortSpawnWithSignal(cmd, time.Second, func(pid int, sig syscall.Signal) error {
				if sig == 0 {
					return probeErr
				}
				return syscall.Kill(pid, sig)
			})
			if !errors.Is(err, probeErr) {
				t.Fatalf("unknown probe was treated as absence: %v", err)
			}
		})
	}
}

func TestAbortSpawn_EscalatesWhenGroupSurvivesDirectChild(t *testing.T) {
	cmd := abortFixture(t)
	var escalated atomic.Bool
	err := abortSpawnWithSignal(cmd, 100*time.Millisecond, func(pid int, sig syscall.Signal) error {
		switch sig {
		case 0:
			// Model a surviving descendant even after the real direct child
			// has been reaped. Only group KILL establishes absence.
			if !escalated.Load() {
				return nil
			}
			return syscall.ESRCH
		case syscall.SIGKILL:
			if pid != -cmd.Process.Pid {
				t.Fatal("escalation did not target the owned group")
			}
			escalated.Store(true)
		}
		return syscall.Kill(pid, sig)
	})
	if err != nil || !escalated.Load() {
		t.Fatalf("parent exit bypassed group escalation: escalated=%v err=%v", escalated.Load(), err)
	}
}
