package gitcredential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

var ErrInterrupted = errors.New("git network operation interrupted before Git reported a result")
var ErrCleanupIncomplete = errors.New("git process cleanup incomplete")

// KnownResultCleanupError reports a verified Git exit with incomplete observation.
// ExitCode zero means a push is known landed; callers must not retry it blindly.
type KnownResultCleanupError struct {
	ExitCode  int
	Operation string
	cause     error
	message   string
}

func (e *KnownResultCleanupError) Error() string { return e.message }
func (e *KnownResultCleanupError) Unwrap() error { return e.cause }

type networkError struct {
	message string
	cause   error
}

func (e *networkError) Error() string { return e.message }
func (e *networkError) Unwrap() error { return e.cause }

var networkTimeout = 10 * time.Minute
var reapGrace = 5 * time.Second
var ioGrace = 2 * time.Second
var newPipe = os.Pipe
var waitLeader = (*exec.Cmd).Wait
var readNetwork = func(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// Hooks are nil outside tests. Tests restore them only after settledExit.
var lastRun func(*networkRun)

type networkRead struct {
	file   *os.File
	done   chan struct{}
	buffer bytes.Buffer
	err    error
}
type networkRun struct {
	cmd                                      *exec.Cmd
	readers                                  []*networkRead
	reaped, waiterExit, settled, settledExit chan struct{}
	deadline                                 time.Time
	waitErr                                  error
	state                                    *os.ProcessState
}

// IsNetwork identifies the bounded coordinator network verbs.
func IsNetwork(args []string) bool {
	return len(args) > 0 && (args[0] == "fetch" || args[0] == "push" || args[0] == "ls-remote")
}

// Network bounds execution, leader reaping and owned pipe cleanup. It signals only
// its unreaped Git leader; ambiguous transports survive and held I/O is reported.
// Start and token resolution have their own execution constraints. A kernel-stuck
// leader can retain the tracked Wait worker until the kernel permits reaping.
// No diagnostic writer or reporting worker is invoked. Returned results are
// snapshots; late reaping cannot change them.
func Network(ctx context.Context, root string, combined bool, args ...string) ([]byte, error) {
	if !IsNetwork(args) {
		return nil, errors.New("not a Git network operation")
	}
	op := args[0]
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("git %s not started: %w", op, err)
	}
	bounded, cancel := context.WithTimeout(ctx, networkTimeout)
	defer cancel()
	cmd, redact, err := Command(bounded, root, args...)
	if err != nil {
		return nil, err
	}
	r := &networkRun{cmd: cmd, reaped: make(chan struct{}), settled: make(chan struct{}), settledExit: make(chan struct{}), waiterExit: make(chan struct{})}
	r.deadline, _ = bounded.Deadline()
	var files []*os.File
	closeFiles := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	pipe := func() (*networkRead, *os.File, error) {
		rd, wr, e := newPipe()
		if e != nil {
			return nil, nil, e
		}
		files = append(files, rd, wr)
		reader := &networkRead{file: rd, done: make(chan struct{})}
		r.readers = append(r.readers, reader)
		return reader, wr, nil
	}
	stdout, write, err := pipe()
	if err != nil {
		closeFiles()
		return nil, &networkError{"git pipe setup failed", err}
	}
	cmd.Stdout = write
	var stderr *networkRead
	if combined {
		cmd.Stderr = write
	} else {
		var ew *os.File
		stderr, ew, err = pipe()
		if err != nil {
			closeFiles()
			return nil, &networkError{"git pipe setup failed", err}
		}
		cmd.Stderr = ew
	}
	if err = cmd.Start(); err != nil {
		closeFiles()
		return nil, &networkError{redact("git " + op + " start failed: " + err.Error()), err}
	}
	// os/exec owns no copy goroutines: both outputs are *os.File.
	for i := 1; i < len(files); i += 2 {
		_ = files[i].Close()
	}
	for _, reader := range r.readers {
		go func(rd *networkRead) {
			defer close(rd.done)
			defer rd.file.Close()
			_, rd.err = readNetwork(&rd.buffer, rd.file)
		}(reader)
	}
	go func() {
		defer close(r.waiterExit)
		r.waitErr = waitLeader(cmd)
		r.state = cmd.ProcessState
		close(r.reaped)
	}()
	go func() {
		defer close(r.settledExit)
		<-r.waiterExit
		for _, rd := range r.readers {
			<-rd.done
		}
		close(r.settled)
	}()
	if lastRun != nil {
		lastRun(r)
	}
	reaped := false
	select {
	case <-r.reaped:
		reaped = true
	case <-bounded.Done():
		timer := time.NewTimer(reapGrace)
		select {
		case <-r.reaped:
			reaped = true
		case <-timer.C:
		}
		timer.Stop()
	}
	// One deadline for all readers, not one grace interval per pipe.
	timer := time.NewTimer(ioGrace)
	held := false
	for _, rd := range r.readers {
		select {
		case <-rd.done:
		case <-timer.C:
			held = true
		}
		if held {
			break
		}
	}
	timer.Stop()
	if held {
		for _, rd := range r.readers {
			_ = rd.file.Close()
		}
	}
	var readErr error
	for _, rd := range r.readers {
		<-rd.done
		if rd.err != nil && !errors.Is(rd.err, os.ErrClosed) {
			readErr = errors.Join(readErr, rd.err)
		}
	}
	output := append([]byte(nil), stdout.buffer.Bytes()...)
	var causes []error
	cleanup := held || readErr != nil || !reaped
	if cleanup {
		causes = append(causes, ErrCleanupIncomplete, readErr)
	}
	known := false
	code := -1
	if reaped {
		causes = append(causes, r.waitErr)
		var exit *exec.ExitError
		verifiedWait := r.waitErr == nil || errors.As(r.waitErr, &exit)
		known = verifiedWait && r.state != nil && r.state.Exited()
		if known {
			code = r.state.ExitCode()
		}
		if exit != nil && stderr != nil {
			exit.Stderr = append([]byte(nil), stderr.buffer.Bytes()...)
		}
	}
	if !known {
		causes = append(causes, ErrInterrupted, bounded.Err())
		if !cleanup && (r.state == nil || (r.waitErr != nil && !isExitError(r.waitErr))) {
			causes = append(causes, ErrCleanupIncomplete)
		}
		guidance := "remote head was not completely observed; rerun"
		if op == "push" {
			guidance = "remote outcome unknown; rerun the same swarm command to reinspect authoritative state and remote head; do not force-push; local candidate retained"
		}
		if op == "fetch" {
			guidance = "no remote refs were changed; rerun when reachable"
		}
		detail := ""
		if !reaped {
			detail = "; Git leader did not exit within reap budget"
		}
		if held {
			detail += "; inheriting process was left running"
		}
		return output, &networkError{"git " + op + " interrupted: " + guidance + detail, errors.Join(causes...)}
	}
	if cleanup {
		message := "git " + op + ": cleanup/observation incomplete"
		if code == 0 {
			message = "git " + op + ": Git completed successfully; cleanup/observation incomplete"
			if op == "push" {
				message += "; publication is known landed; do not retry blindly"
			}
		}
		if held {
			message += "; inheriting process was left running"
		}
		return output, &KnownResultCleanupError{ExitCode: code, Operation: op, cause: errors.Join(causes...), message: message}
	}
	if r.waitErr != nil {
		return output, &networkError{redact("git " + op + ": " + r.waitErr.Error()), r.waitErr}
	}
	return output, nil
}

func isExitError(err error) bool { var exit *exec.ExitError; return errors.As(err, &exit) }
