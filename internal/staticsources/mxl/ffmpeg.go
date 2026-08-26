package mxl

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// ffmpegProcess owns an ffmpeg subprocess and the choreography that shuts it
// down.
//
// Both encoders here are the same shape: write raw media into stdin, read
// encoded media back off stdout, and stop cleanly whether ffmpeg exits first,
// the reader errors first, or the owner calls Close. Only the reading differs
// -- Annex-B access units for H.264, Ogg pages for Opus -- so that is the one
// part injected.
//
// Talking to ffmpeg over pipes rather than linking the codecs keeps GPL code
// out of this binary, and keeps its ability to start independent of which
// shared libraries the base image happens to carry.
type ffmpegProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	// readAll consumes stdout until EOF or error. It runs on its own
	// goroutine and its return value becomes the terminal error unless
	// something else ended the process first.
	readAll func() error

	finalErr  error
	terminate chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	closed    bool
}

// startFFmpeg launches ffmpeg with the given arguments. readAll is set by the
// caller before the first Write, and run() is started by the caller once it
// is.
func startFFmpeg(path string, args []string) (*ffmpegProcess, error) {
	cmd := exec.Command(path, args...) //nolint:gosec // operator-controlled
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Warnings surface where the operator expects them.
	cmd.Stderr = os.Stderr
	// Own process group, so a SIGINT to mediamtx does not reach the child
	// before there is a well-defined moment to kill it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	err = cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}

	return &ffmpegProcess{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		terminate: make(chan struct{}),
		done:      make(chan struct{}),
	}, nil
}

// write pushes bytes into ffmpeg's stdin. A blocked write is the encoder
// applying backpressure, which is the intended way for a slow encoder to slow
// its producer down.
func (f *ffmpegProcess) write(b []byte) error {
	_, err := f.stdin.Write(b)
	if err != nil {
		return fmt.Errorf("write to ffmpeg: %w", err)
	}
	return nil
}

// Close shuts the process down. Safe to call multiple times; blocks until the
// goroutine has exited.
func (f *ffmpegProcess) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		<-f.done
		return
	}
	f.closed = true
	f.mu.Unlock()
	close(f.terminate)
	<-f.done
}

// Wait blocks until the goroutine exits and returns the terminal error, nil
// on a clean shutdown.
func (f *ffmpegProcess) Wait() error {
	<-f.done
	return f.finalErr
}

func (f *ffmpegProcess) run() {
	defer close(f.done)
	f.finalErr = f.runInner()
}

// runInner is the shutdown choreography. Three things end the process, and
// each branch closes the pipes in the order that lets the other goroutines
// unblock before reporting a terminal error.
func (f *ffmpegProcess) runInner() error {
	cmdDone := make(chan error, 1)
	go func() { cmdDone <- f.cmd.Wait() }()

	readDone := make(chan error, 1)
	go func() { readDone <- f.readAll() }()

	select {
	case err := <-cmdDone:
		// ffmpeg exited on its own; close the pipes so the reader unblocks.
		_ = f.stdin.Close()
		_ = f.stdout.Close()
		<-readDone
		if err != nil {
			return fmt.Errorf("ffmpeg exited: %w", err)
		}
		return fmt.Errorf("ffmpeg exited unexpectedly")

	case err := <-readDone:
		_ = f.stdin.Close()
		<-cmdDone
		_ = f.stdout.Close()
		return err

	case <-f.terminate:
		// Closing stdin lets ffmpeg flush and exit on EOF. The signal is
		// belt and braces for a build that ignores the close.
		_ = f.stdin.Close()
		if f.cmd.Process != nil {
			_ = syscall.Kill(-f.cmd.Process.Pid, syscall.SIGINT)
		}
		<-cmdDone
		_ = f.stdout.Close()
		<-readDone
		return nil
	}
}
