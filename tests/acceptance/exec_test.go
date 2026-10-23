package acceptance

import (
	"bytes"
	"os/exec"
)

// execCmd is a tiny wrapper so the harness can build a command without importing
// os/exec in every file.
type execCmd struct{ *exec.Cmd }

func newExecCmd(bin string, args ...string) *execCmd {
	cmd := exec.Command(bin, args...)
	return &execCmd{Cmd: cmd}
}

func (c *execCmd) CombinedOutput() ([]byte, error) {
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	return out.Bytes(), err
}
