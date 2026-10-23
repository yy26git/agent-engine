package runtime

import "context"

// SessionOutput carries identity measured by the owning transport.
type SessionOutput struct{ SessionID, Text, Model string }
type AttributedOutputReader interface {
	ReadAttributedOutput(context.Context, SessionRef, int) (SessionOutput, error)
}

// QuiescenceProver supplements session status with owned-process evidence.
type QuiescenceProver interface {
	ProveQuiescence(context.Context, SessionRef) (bool, error)
}
