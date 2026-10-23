package minimaxacp

import (
	"os/exec"
	"testing"
	"time"
)

func TestWaitForDescendants_ObservesExitWithinBound(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "IFS= read -r line; exit 0")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); <-done })
	// The fixture is held alive by its input pipe and then independently exits,
	// as a detached guardian does after observing parent IPC disconnect.
	timer := time.AfterFunc(50*time.Millisecond, func() { _ = input.Close() })
	defer timer.Stop()
	if left := waitForDescendants([]int{pid}, 2*time.Second); len(left) != 0 {
		t.Fatalf("exited descendant retained as alive: %v", left)
	}
}

func TestWaitForDescendants_StillAliveAtDeadlineRemainsUnproven(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "IFS= read -r line; exit 0")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	left := waitForDescendants([]int{cmd.Process.Pid}, 50*time.Millisecond)
	if len(left) != 1 || left[0] != cmd.Process.Pid {
		t.Fatalf("live descendant was hidden: %v", left)
	}
}
