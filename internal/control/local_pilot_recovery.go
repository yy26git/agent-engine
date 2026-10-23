package control

import (
	"strings"
	"syscall"
)

// retiredLocalACPSession recognises an execution host already retired by the
// supervisor/reaper. It never starts a transport, signals an unverified PID or
// claims the retained native chat was deleted. This is trusted-local recovery,
// not evidence about processes deliberately detached from the recorded group.
func (e *Engine) retiredLocalACPSession(requestID, sessionID string) bool {
	s, err := e.store.State(requestID)
	if err != nil || s.ExecutionAssurance != AssuranceTrustedLocal || s.LocalPilot == nil {
		return false
	}
	b, ok := s.BackendForID(sessionID)
	if !ok || (b.Backend != "minimax-acp" && !strings.HasPrefix(b.Backend, "minimax-acp:")) || b.ProcessPID <= 1 || b.ProcessStart == "" {
		return false
	}
	matched := false
	expectedBackend := "minimax-acp:" + s.LocalPilot.WorkerProfile
	if b.Backend != "minimax-acp" && b.Backend != expectedBackend {
		return false
	}
	for name, rec := range s.OwnedProcesses {
		if rec.Pending {
			return false
		}
		if name == expectedBackend && rec.PID == b.ProcessPID && rec.StartToken == b.ProcessStart {
			matched = true
		}
	}
	if !matched {
		return false
	}
	alive, known := recordedProcessAlive(b.ProcessPID, b.ProcessStart)
	if !known || alive {
		return false
	}
	// The leader being gone is insufficient when its children remain in the
	// process group. EPERM and every uncertain result refuse recovery.
	return syscall.Kill(-b.ProcessPID, 0) == syscall.ESRCH
}
