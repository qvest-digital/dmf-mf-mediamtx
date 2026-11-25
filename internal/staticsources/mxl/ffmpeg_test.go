package mxl

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFFmpegProcessStopsWhenTheReaderFails(t *testing.T) {
	if _, err := exec.LookPath("head"); err != nil {
		t.Skip("head not on PATH")
	}
	// A writer with more pending than a pipe buffer holds, and a reader that
	// gives up: the writer must be stopped, not waited for while it blocks
	// on a pipe nobody drains.
	p, err := startFFmpeg("head", []string{"-c", "4000000", "/dev/zero"})
	require.NoError(t, err)
	readErr := errors.New("parse error")
	p.readAll = func() error { return readErr }
	go p.run()

	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err = <-done:
		require.ErrorIs(t, err, readErr)
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatal("process not stopped after the reader failed")
	}
}
