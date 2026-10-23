package agentdeck

import "bytes"

// Consume excess output so the child can exit normally, but retain only the
// explicit bound and refuse the document. A partial JSON document is never used.
type cappedCommandBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedCommandBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
