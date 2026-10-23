package agentdeck

// The public event stream.
//
// Two properties drive this file, both learned from how the stream actually
// behaves rather than from how it is documented to behave.
//
// First, the stream is not guaranteed complete. The producer's queue can drop
// events, some event sources are opt-in, and daemon restart can miss work that
// already finished. Cursor replay therefore proves that the frames which were
// committed are delivered — it says nothing about whether every worker action
// produced a frame. This package never infers that an unchanged obligation means
// no progress; absence of a frame is UNKNOWN, and a supervisor has to reconcile
// rather than assume.
//
// Second, filtering hides cursors. `events follow` can filter by kind or session,
// but a filtered frame does not advance the caller's position, so a supervisor
// that filters would checkpoint forever behind frames it chose to ignore. This
// follower therefore takes the stream unfiltered and filters after the fact,
// while still reporting every frame's cursor so the checkpoint always advances.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Frame is one event from Agent Deck's public NDJSON stream.
//
// It mirrors the public frame shape. It is a local DTO rather than an import of
// Agent Deck's internal events package, because that package is an unversioned
// implementation detail and importing it would defeat the point of a separate
// module.
type Frame struct {
	Cursor    uint64          `json:"cursor"`
	EventID   string          `json:"event_id"`
	TS        int64           `json:"ts"`
	Kind      string          `json:"kind"`
	SessionID string          `json:"session_id"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// Payload decodes the frame's data into v.
//
// A frame with no payload is reported as an error rather than a zero value: a
// caller that asked for the payload of a frame that has none must be able to
// tell the difference between "absent" and "empty".
func (f Frame) Payload(v any) error {
	if len(f.Data) == 0 {
		return errors.New("agentdeck: frame carries no payload")
	}
	if err := json.Unmarshal(f.Data, v); err != nil {
		return fmt.Errorf("agentdeck: decoding frame %d payload: %w", f.Cursor, err)
	}
	return nil
}

// ErrRetentionGap reports that the recorded cursor predates every retained frame.
//
// This is fatal to automatic progress, not a transient read failure. The frames
// between the checkpoint and the oldest survivor are gone, and they may have
// contained the worker's completion. Resuming from the oldest surviving frame
// would look like progress while silently skipping the evidence the control layer
// exists to act on.
var ErrRetentionGap = errors.New("agentdeck: the event stream no longer retains frames back to the recorded cursor")

// ErrMalformedFrame reports a line that is not a decodable frame.
var ErrMalformedFrame = errors.New("agentdeck: malformed event frame")

// Event kinds the control layer reacts to. These are the notifier's published
// kinds; anything else is carried and checkpointed but not acted on.
const (
	KindSessionTransition = "session.transition"
	KindSessionFinished   = "session.finished"
	KindSessionStatus     = "session.status"
	KindSessionTurn       = "session.turn"
)

// Events follows the unfiltered public stream from a cursor.
//
// The returned channels are closed when ctx is cancelled or the follower exits.
// The error channel carries the reason; a follower that ends with an error must
// not be treated as having reached the end of the stream.
func (r *CLIRuntime) Events(ctx context.Context, after uint64) (<-chan Frame, <-chan error, error) {
	bin, err := r.binary()
	if err != nil {
		return nil, nil, err
	}
	health, err := r.BusHealth(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("agentdeck: checking bus health before following: %w", err)
	}
	if !health.OK() {
		return nil, nil, fmt.Errorf("agentdeck: the event bus is disabled; the control layer would see nothing while appearing healthy")
	}
	// A cursor beyond the stream's current position is almost certainly from a
	// different store or profile, and following from it would silently skip the
	// whole log.
	if after > health.Cursor {
		return nil, nil, fmt.Errorf("agentdeck: recorded cursor %d is ahead of the stream's current position %d; refusing to skip the log",
			after, health.Cursor)
	}

	// No --kind and no --session: the stream is taken unfiltered so every frame's
	// cursor is visible and the checkpoint can always advance.
	args := []string{"events", "follow", "--json", fmt.Sprintf("--after=%d", after)}
	if strings.TrimSpace(r.Profile) != "" {
		args = append([]string{"-p", r.Profile}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("agentdeck: opening event stream: %w", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("agentdeck: starting event stream: %w", err)
	}

	frames := make(chan Frame, 256)
	errs := make(chan error, 1)

	var once sync.Once
	finish := func(err error) {
		once.Do(func() {
			if err != nil {
				errs <- err
			}
			close(errs)
			close(frames)
		})
	}

	go func() {
		defer func() {
			// The follower is owned by this goroutine. On cancellation the child is
			// killed and waited for, so following a stream never leaks a process
			// that keeps writing into a store nobody is reading.
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()

		scanner := bufio.NewScanner(stdout)
		// NDJSON lines carrying a large payload must not be silently truncated at
		// the default limit, because a truncated line decodes into a frame that
		// looks valid but describes something else.
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

		var lastCursor uint64
		lines := 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			lines++
			var f Frame
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				// A malformed line is reported and the stream continues. The bus
				// is shared and a future producer may emit a shape this build does
				// not know; one unknown line is not evidence that the log is gone.
				if r.Log != nil {
					r.Log(fmt.Sprintf("events: skipping malformed line %d: %v", lines, err))
				}
				continue
			}
			// Cursors must advance monotonically. A frame that moves backwards
			// would let a later commit rewind the checkpoint and re-decide work.
			if f.Cursor <= lastCursor && lastCursor != 0 {
				if r.Log != nil {
					r.Log(fmt.Sprintf("events: ignoring non-advancing cursor %d after %d", f.Cursor, lastCursor))
				}
				continue
			}
			lastCursor = f.Cursor
			select {
			case frames <- f:
			case <-ctx.Done():
				finish(ctx.Err())
				return
			}
		}

		if err := scanner.Err(); err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				finish(ctx.Err())
				return
			}
			finish(fmt.Errorf("agentdeck: reading event stream: %w", err))
			return
		}
		if ctx.Err() != nil {
			finish(ctx.Err())
			return
		}
		// The stream ended on its own. That is a follower exit, not a completed
		// request, and a supervisor must reconcile rather than conclude the work
		// is finished.
		finish(nil)
	}()

	return frames, errs, nil
}

// DrainTo reads frames until the cursor passes a boundary, or the stream ends.
//
// It exists for a deterministic single pass: a caller that wants "process
// everything already published, then stop" needs a stopping rule that does not
// depend on a sleep. Every frame advances the boundary check, including frames
// belonging to other sessions and frames that are not workflow events, so an
// all-irrelevant backlog still terminates rather than blocking forever.
func DrainTo(ctx context.Context, frames <-chan Frame, errs <-chan error, boundary uint64, emit func(Frame) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errs:
			if ok && err != nil {
				return err
			}
			if !ok {
				errs = nil
			}
		case f, ok := <-frames:
			if !ok {
				// The stream ended. Report it as a follower exit so a caller
				// cannot mistake it for having processed everything.
				select {
				case err := <-errs:
					if err != nil {
						return err
					}
				default:
				}
				return io.EOF
			}
			if err := emit(f); err != nil {
				return err
			}
			if boundary > 0 && f.Cursor >= boundary {
				return nil
			}
		}
	}
}
