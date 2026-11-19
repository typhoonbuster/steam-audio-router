package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// PwDump supervises a `pw-dump -N [-m]` child and turns its output (a stream
// of concatenated JSON arrays) into batches of decoded objects.
type PwDump struct {
	cmd     *exec.Cmd
	Batches chan []pwObject // closed when the child's output ends
	errCh   chan error
	stderr  bytes.Buffer
	done    bool
	exitErr error
}

func startPwDump(monitor bool) (*PwDump, error) {
	args := []string{"-N"}
	if monitor {
		args = append(args, "-m")
	}
	d := &PwDump{cmd: exec.Command("pw-dump", args...), Batches: make(chan []pwObject, 8), errCh: make(chan error, 1)}
	stdout, err := d.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	d.cmd.Stderr = &d.stderr
	if err := d.cmd.Start(); err != nil {
		return nil, err
	}
	go d.readLoop(stdout)
	return d, nil
}

func (d *PwDump) readLoop(stdout io.Reader) {
	dec := json.NewDecoder(stdout)
	var readErr error
	for {
		var raw []pwRaw
		if err := dec.Decode(&raw); err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
		d.Batches <- decodeBatch(raw)
	}
	close(d.Batches)
	waitErr := d.cmd.Wait()
	switch {
	case readErr != nil:
		d.errCh <- fmt.Errorf("decode error: %w", readErr)
	case waitErr != nil:
		msg := strings.TrimSpace(d.stderr.String())
		if msg != "" {
			d.errCh <- fmt.Errorf("%v: %s", waitErr, msg)
		} else {
			d.errCh <- waitErr
		}
	default:
		d.errCh <- nil
	}
}

// Wait blocks until the child exited and returns its error (cached).
func (d *PwDump) Wait() error {
	if !d.done {
		d.exitErr = <-d.errCh
		d.done = true
	}
	return d.exitErr
}

// Stop terminates the child (SIGTERM, then SIGKILL after 3 s) and reaps it.
func (d *PwDump) Stop() {
	if d.done || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-d.errCh:
		d.exitErr, d.done = err, true
	case <-time.After(3 * time.Second):
		_ = d.cmd.Process.Kill()
		d.exitErr, d.done = <-d.errCh, true
	}
	// Drain anything the reader produced before exiting.
	for range d.Batches {
	}
}
