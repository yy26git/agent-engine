package control

import (
	"fmt"
	"strings"
)

const (
	SupervisorRunning         = "running"
	SupervisorStopped         = "stopped"
	SupervisorIntentionalStop = "intentional_service_stop"
	SupervisorSignalStop      = "intentional_signal_stop"
	SupervisorNormalExit      = "normal_exit"
	SupervisorAbruptDeath     = "abrupt_process_death"
)

// SupervisorLifecycle is the durable, measured identity of the process that
// currently owns (or most recently owned) a request. It is metadata only; the
// request flock remains the authority for exclusive execution.
type SupervisorLifecycle struct {
	PID             int    `json:"pid"`
	StartToken      string `json:"start_token"`
	StartedAt       string `json:"started_at"`
	State           string `json:"state"`
	StopRequestedAt string `json:"stop_requested_at,omitempty"`
	StopReason      string `json:"stop_reason,omitempty"`
	LastExitKind    string `json:"last_exit_kind,omitempty"`
	LastExitAt      string `json:"last_exit_at,omitempty"`
}

// BeginSupervisor commits process identity after Engine has acquired the
// request ownership lock. A prior running record can therefore be classified
// as an exit only after exclusive ownership has been re-established.
func (s *Store) BeginSupervisor(requestID string, pid int, startToken, at string) error {
	if pid <= 1 || strings.TrimSpace(startToken) == "" || strings.TrimSpace(at) == "" {
		return fmt.Errorf("agent-engine: supervisor start requires a measured pid, start token and timestamp")
	}
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		lastKind, lastAt := "", ""
		if prior := st.Supervisor; prior != nil {
			lastKind, lastAt = prior.LastExitKind, prior.LastExitAt
			if prior.State == SupervisorRunning {
				lastAt = at
				if prior.StopRequestedAt != "" {
					lastKind = SupervisorIntentionalStop
				} else {
					lastKind = SupervisorAbruptDeath
				}
			}
		}
		st.Supervisor = &SupervisorLifecycle{
			PID: pid, StartToken: startToken, StartedAt: at, State: SupervisorRunning,
			LastExitKind: lastKind, LastExitAt: lastAt,
		}
		return nil
	})
	return err
}

// RequestSupervisorStop durably records the service's intent before it sends
// a termination signal. The pid and measured start token prevent a stale
// service entry from marking a different process as intentionally stopped.
func (s *Store) RequestSupervisorStop(requestID string, pid int, startToken, reason, at string) error {
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		cur := st.Supervisor
		if cur == nil || cur.State != SupervisorRunning || cur.PID != pid || cur.StartToken != startToken {
			return fmt.Errorf("agent-engine: supervisor stop identity no longer matches the recorded owner")
		}
		cur.StopRequestedAt = at
		cur.StopReason = strings.TrimSpace(reason)
		return nil
	})
	return err
}

// EndSupervisor records a handled exit before Engine releases its ownership
// lock. If the process dies before this transaction, the next owner recognizes
// the still-running record as a process death (or an intentional stop when the
// service's stop intent was already committed).
func (s *Store) EndSupervisor(requestID string, pid int, startToken, exitKind, at string) error {
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		cur := st.Supervisor
		if cur == nil || cur.State != SupervisorRunning || cur.PID != pid || cur.StartToken != startToken {
			return fmt.Errorf("agent-engine: supervisor exit identity no longer matches the recorded owner")
		}
		if cur.StopRequestedAt != "" {
			exitKind = SupervisorIntentionalStop
		}
		cur.State = SupervisorStopped
		cur.LastExitKind = exitKind
		cur.LastExitAt = at
		return nil
	})
	return err
}
