package gitcredential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
	message    string
	cause      error
	diagnostic string
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
	return network(ctx, root, combined, 0, args...)
}

// NetworkLimited uses the same process lifecycle as Network, with a fixed
// output cap per stream. Exceeding the cap cancels the operation before the
// buffer grows; callers reserve both stream budgets before starting Git.
func NetworkLimited(ctx context.Context, root string, combined bool, maxBytes int, args ...string) ([]byte, error) {
	if maxBytes < 1 {
		return nil, errors.New("positive Git output limit required")
	}
	return network(ctx, root, combined, maxBytes, args...)
}
func networkAttempt(ctx context.Context, root string, combined bool, maxBytes int, args ...string) ([]byte, error) {
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
		return nil, &networkError{"git pipe setup failed", err, ""}
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
			return nil, &networkError{"git pipe setup failed", err, ""}
		}
		cmd.Stderr = ew
	}
	if err = cmd.Start(); err != nil {
		closeFiles()
		return nil, &networkError{redact("git " + op + " start failed: " + err.Error()), err, ""}
	}
	// os/exec owns no copy goroutines: both outputs are *os.File.
	for i := 1; i < len(files); i += 2 {
		_ = files[i].Close()
	}
	for _, reader := range r.readers {
		go func(rd *networkRead) {
			defer close(rd.done)
			defer rd.file.Close()
			var dst io.Writer = &rd.buffer
			if maxBytes > 0 {
				dst = &networkOutputLimit{buffer: &rd.buffer, remaining: maxBytes, cancel: cancel}
			}
			_, rd.err = readNetwork(dst, rd.file)
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
		return output, &networkError{"git " + op + " interrupted: " + guidance + detail, errors.Join(causes...), ""}
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
	diagnostic := string(output)
	if stderr != nil {
		diagnostic += stderr.buffer.String()
	}
	if r.waitErr != nil {
		return output, &networkError{redact("git " + op + ": " + r.waitErr.Error()), r.waitErr, diagnostic}
	}
	return output, nil
}

func isExitError(err error) bool { var exit *exec.ExitError; return errors.As(err, &exit) }

type networkOutputLimit struct {
	buffer    *bytes.Buffer
	remaining int
	cancel    context.CancelFunc
}

func (w *networkOutputLimit) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.cancel()
		return 0, errors.New("git inventory output limit exceeded")
	}
	w.remaining -= len(p)
	return w.buffer.Write(p)
}

// RetryObservation contains classification evidence, never user-facing diagnostics.
type RetryObservation struct {
	Operation      string
	Err            error
	Diagnostic     string
	Status         int
	Header         http.Header
	SecondaryLimit bool
}

const RetryAttempts = 4
const retryDelayBudget = time.Minute

// RetryReason is deliberately conservative for mutations: only positive evidence
// that the connection was never established permits replay.
func RetryReason(o RetryObservation) string {
	if errors.Is(o.Err, context.Canceled) || errors.Is(o.Err, ErrInterrupted) || errors.Is(o.Err, ErrCleanupIncomplete) {
		return ""
	}
	var known *KnownResultCleanupError
	if errors.As(o.Err, &known) {
		return ""
	}
	d := strings.ToLower(o.Diagnostic)
	for _, fatal := range []string{"authentication", "permission denied", "access denied", "repository not found", "rejected", "stale info", "non-fast-forward", "failed to push"} {
		if strings.Contains(d, fatal) {
			return ""
		}
	}
	statuses := gitHTTPStatus.FindAllStringSubmatch(d, -1)
	for _, match := range statuses {
		code, _ := strconv.Atoi(match[1])
		if code == 401 || code == 403 || code == 404 || (o.Operation == "push" && code >= 500) {
			return ""
		}
	}
	if gitMissingRepository.MatchString(d) {
		return ""
	}
	if o.Operation == "push" {
		for _, ambiguous := range []string{"connection reset", "remote:", "unexpected disconnect", "empty reply", "eof", "broken pipe", "send failure", "recv failure", "connected to", "upload", "sent off", "writing objects", "receiving objects", "remote end hung up"} {
			if strings.Contains(d, ambiguous) {
				return ""
			}
		}
	}
	read := o.Operation == "fetch" || o.Operation == "ls-remote" || o.Operation == "GET"
	if o.Status == 401 || o.Status == 404 || (o.Status == 403 && (!read || (o.Header.Get("Retry-After") == "" && o.Header.Get("X-RateLimit-Remaining") != "0" && !o.SecondaryLimit))) {
		return ""
	}
	if !read && o.Status != 0 {
		return ""
	}
	var dns *net.DNSError
	var op *net.OpError
	// A DNS cause nested in a read/write error does not prove that no
	// request was sent. The outer network operation takes precedence.
	connect := errors.As(o.Err, &dns)
	if errors.As(o.Err, &op) {
		connect = op.Op == "dial"
	}
	gitConnect := strings.Contains(d, "could not resolve host") || strings.Contains(d, "failed to connect to")
	if connect || gitConnect {
		return "connection unavailable"
	}
	if !read {
		return ""
	}
	if o.Status == 429 || (o.Status == 403 && (o.Header.Get("Retry-After") != "" || o.Header.Get("X-RateLimit-Remaining") == "0" || o.SecondaryLimit)) {
		return "rate limited"
	}
	if o.Status == 502 || o.Status == 503 || o.Status == 504 {
		return "service unavailable"
	}
	if o.Err != nil && o.Operation == "GET" {
		var networkErr net.Error
		if errors.As(o.Err, &networkErr) || errors.Is(o.Err, io.EOF) || errors.Is(o.Err, io.ErrUnexpectedEOF) || errors.Is(o.Err, syscall.ECONNRESET) || errors.Is(o.Err, syscall.EPIPE) {
			return "connection interrupted"
		}
	}
	for _, match := range statuses {
		if match[1][0] == '5' {
			return "connection interrupted"
		}
	}
	for _, transient := range []string{"connection timed out", "connection reset"} {
		if strings.Contains(d, transient) {
			return "connection interrupted"
		}
	}
	return ""
}

var gitHTTPStatus = regexp.MustCompile(`(?:http(?:/[0-9.]+)?\s+|(?:requested url returned )?error:\s*)([1-5][0-9]{2})(?:[^0-9]|$)`)
var gitMissingRepository = regexp.MustCompile(`repository ['"].*['"] not found`)

type retryContextKey struct{}
type retryContext struct {
	command string
	notify  func(string, int, int, time.Duration)
}

// WithNetworkRetry labels failures with a registered rerun command and routes
// retry notices to the command's serialized progress observer. A nil observer
// suppresses human output for machine-readable commands.
func WithNetworkRetry(ctx context.Context, command string, notify func(string, int, int, time.Duration)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, retryContextKey{}, retryContext{command, notify})
}

var retryNow = time.Now
var retryJitter = func(ceiling time.Duration) time.Duration {
	return ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1))
}
var retryWait = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RetryPolicy owns the attempt and cumulative wait caps for one logical call.
type RetryPolicy struct {
	Attempts  int
	delay     time.Duration
	exhausted bool
}

func (p *RetryPolicy) Next(ctx context.Context, o RetryObservation) (bool, error) {
	p.exhausted = false
	p.Attempts++
	reason := RetryReason(o)
	if reason == "" {
		return false, nil
	}
	if p.Attempts >= RetryAttempts {
		p.exhausted = true
		return false, nil
	}
	delay := retryJitter(time.Duration(1<<(p.Attempts-1)) * 8 * time.Second)
	if value := o.Header.Get("Retry-After"); value != "" {
		requested := time.Duration(0)
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			if seconds > 60 {
				p.exhausted = true
				return false, nil
			}
			if seconds > 0 {
				requested = time.Duration(seconds) * time.Second
			}
		} else if strings.Trim(value, "0123456789") == "" {
			p.exhausted = true
			// An overflowing delta-seconds value still requests a wait beyond
			// our budget; treating it as malformed would retry too early.
			return false, nil
		} else if date, err := http.ParseTime(value); err == nil {
			requested = date.Sub(retryNow())
		}
		if requested > delay {
			delay = requested
		}
	}
	if delay > retryDelayBudget-p.delay {
		p.exhausted = true
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	p.delay += delay
	if reporting, ok := ctx.Value(retryContextKey{}).(retryContext); ok {
		if reporting.notify != nil {
			reporting.notify(reason, p.Attempts+1, RetryAttempts, delay)
		}
	} else {
		fmt.Fprintf(os.Stderr, "GitHub unreachable (%s); retry %d/%d in %s\n", reason, p.Attempts+1, RetryAttempts, delay.Round(time.Second))
	}
	if err := retryWait(ctx, delay); err != nil {
		return false, err
	}
	return true, nil
}

// RetryExhaustedError marks a retryable failure stopped by the shared caps.
// Its cause remains available for errors.Is and errors.As.
type RetryExhaustedError struct {
	Operation string
	Attempts  int
	cause     error
}

func (e *RetryExhaustedError) Error() string { return e.cause.Error() }
func (e *RetryExhaustedError) Unwrap() error { return e.cause }

// Exhausted reports whether the last observation reached an attempt or wait cap.
func (p *RetryPolicy) Exhausted() bool { return p.exhausted }

func (p *RetryPolicy) Error(ctx context.Context, operation string, err error) error {
	command := "the same swarm command"
	if reporting, ok := ctx.Value(retryContextKey{}).(retryContext); ok && reporting.command != "" {
		command = reporting.command
	}
	// Keep the cause for errors.Is/As without exposing transport URLs or bodies.
	redaction, redactErr := NewRedaction()
	if redactErr != nil {
		command = "the same swarm command"
		operation = "network operation"
	} else {
		command = redaction.Redact(command)
		operation = redaction.Redact(operation)
	}
	failure := &networkError{fmt.Sprintf("%s failed after %d attempt(s); rerun %s", operation, p.Attempts, command), err, ""}
	if p.exhausted && ctx.Err() == nil {
		return &RetryExhaustedError{Operation: operation, Attempts: p.Attempts, cause: failure}
	}
	return failure
}

func network(ctx context.Context, root string, combined bool, maxBytes int, args ...string) ([]byte, error) {
	if !IsNetwork(args) {
		return nil, errors.New("not a Git network operation")
	}
	var policy RetryPolicy
	for {
		output, err := networkAttempt(ctx, root, combined, maxBytes, args...)
		if err == nil {
			return output, nil
		}
		diagnostic := ""
		var failure *networkError
		if errors.As(err, &failure) {
			diagnostic = failure.diagnostic
		}
		again, waitErr := policy.Next(ctx, RetryObservation{Operation: args[0], Err: err, Diagnostic: diagnostic})
		if waitErr != nil {
			return output, policy.Error(ctx, "git "+args[0], waitErr)
		}
		if !again {
			// Preserve established lifecycle diagnostics and typed error chains.
			if policy.Attempts == 1 && !policy.exhausted {
				return output, err
			}
			return output, policy.Error(ctx, "git "+args[0], err)
		}
	}
}

// WithRetryObserver replaces delivery while retaining the originating command.
func WithRetryObserver(ctx context.Context, notify func(string, int, int, time.Duration)) context.Context {
	reporting, _ := ctx.Value(retryContextKey{}).(retryContext)
	reporting.notify = notify
	return context.WithValue(ctx, retryContextKey{}, reporting)
}
