// Copyright (c) 2025 Visvasity LLC

// Package unixlock provides inter-process mutual exclusion using Unix domain sockets.
// It creates a cooperative mutex at a specified file path, allowing processes to:
//
//   - Acquire and release locks.
//   - Communicate with the lock owner to request graceful shutdown.
//   - Report success or failure to the foreground process from the background process.
//   - Query the owner's process ID (PID).
//   - Verify if the owner is an ancestor process.
//
// The mutex supports non-blocking and blocking lock acquisition, context-aware
// cancellation, and status reporting between processes.
package unixlock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrShutdown indicates the context was canceled due to a shutdown request.
var ErrShutdown = errors.New("shutdown")

// ErrUnlocked indicates the context was canceled because the lock was closed.
var ErrUnlocked = errors.New("unlocked")

// handleTimeout bounds how long the lock server spends serving a single
// connection. Requests are tiny and local, so a slow or silent client should
// never be able to stall the accept goroutine. It is a var (not a const) so
// tests can shorten it.
var handleTimeout = 5 * time.Second

type Mutex struct {
	mu sync.Mutex

	wg sync.WaitGroup

	listener atomic.Pointer[net.UnixListener]

	derivedCancels []context.CancelCauseFunc

	fpath string

	ppid int

	pidMap map[int]bool

	reportCh chan string

	reported bool
}

// New creates a cooperative mutual exclusion lock instance using a Unix domain
// socket at the specified file path. The mutex supports inter-process
// communication for shutdown requests, PID queries, and ancestor checks.
//
// New records the current process's parent PID (os.Getppid) at construction;
// Report uses it to address the lock owner, so a Mutex intended for reporting
// must be created while the parent that owns the lock is still the caller's
// parent (see Report).
func New(fpath string) *Mutex {
	m := &Mutex{
		fpath:    fpath,
		ppid:     os.Getppid(),
		pidMap:   make(map[int]bool),
		reportCh: make(chan string, 1),
	}
	m.pidMap[os.Getpid()] = true
	return m
}

// Close releases the lock if held by the current process and cancels all derived
// contexts with os.ErrClosed. It waits for all associated goroutines to terminate.
func (m *Mutex) Close() {
	m.stopServer(os.ErrClosed)
	m.wg.Wait()
}

// SocketPath returns the monitor socket path.
func (m *Mutex) SocketPath() string {
	return m.fpath
}

func (m *Mutex) startServer(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.listener.Load() != nil {
		return "", os.ErrExist
	}

	dir := filepath.Dir(m.fpath)
	base := filepath.Base(m.fpath)
	pid := os.Getpid()

	tpath := filepath.Join(dir, fmt.Sprintf("%s.%d", base, pid))
	_ = os.Remove(tpath) // Remove stale file with matching pid (very unlikely)

	addr := &net.UnixAddr{Name: tpath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return "", err
	}

	m.listener.Store(l)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		for l := m.listener.Load(); l != nil; l = m.listener.Load() {
			conn, err := l.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					slog.Error("could not accept incoming connection (ignored)", "err", err)
					continue
				}
				return
			}
			m.handle(conn)
		}
	}()

	return tpath, nil
}

func (m *Mutex) stopServer(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if l := m.listener.Load(); l != nil {
		l.Close()
		m.listener.Store(nil)
		for _, cancel := range m.derivedCancels {
			cancel(err)
		}
		m.derivedCancels = nil
	}
}

func (m *Mutex) handle(conn net.Conn) {
	defer conn.Close()

	// Bound the whole exchange so a slow or silent client cannot stall the
	// (single-threaded) accept goroutine and wedge the lock server.
	conn.SetDeadline(time.Now().Add(handleTimeout))

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		slog.Warn("could not read from incoming connection", "err", err)
		return
	}
	request := string(buf[:n])
	args := strings.Fields(request)
	if len(args) == 0 {
		writeErr(conn, "empty command")
		slog.Warn("received empty/whitespace-only command", "socket", m.fpath)
		return
	}
	cmd := args[0]

	switch cmd {
	case "getpid":
		writeOK(conn, strconv.Itoa(os.Getpid()))
		return

	case "shutdown":
		// An optional target PID lets the sender address a specific owner: if it
		// does not match this process, a newcomer that took the socket after the
		// sender observed the previous owner ignores the request. That is still
		// an OK reply, since from the sender's view the intended owner is gone.
		if len(args) >= 2 {
			if pid, err := strconv.ParseInt(args[1], 10, 64); err == nil && int(pid) != os.Getpid() {
				slog.Info("ignoring shutdown addressed to a different pid", "target", pid, "self", os.Getpid(), "socket", m.fpath)
				writeOK(conn, "")
				return
			}
		}
		slog.Info("canceling derived contexts due to shutdown message", "socket", m.fpath)
		// Snapshot the cancel funcs under the lock, then invoke them without
		// holding it. WithLock appends to this slice concurrently.
		m.mu.Lock()
		cancels := m.derivedCancels
		m.mu.Unlock()
		for _, cancel := range cancels {
			cancel(ErrShutdown)
		}
		writeOK(conn, "")
		return

	case "report":
		slog.Debug("received a report", "socket", m.fpath, "args", args)
		if len(args) < 2 {
			writeErr(conn, "invalid arguments")
			return
		}
		report := ""
		if len(args) > 2 {
			report = strings.Join(args[2:], " ")
		}
		pid, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || pid != int64(os.Getpid()) {
			if err == nil {
				slog.Warn("report message target is not the current lock owner", "report", report, "target", pid, "socket", m.fpath)
			}
			writeErr(conn, "invalid arguments")
			return
		}
		// Non-blocking send: we only care about the first report. If a report
		// is already buffered (or no one is waiting), drop this one rather than
		// block the accept goroutine, which would wedge the whole lock server.
		select {
		case m.reportCh <- report:
		default:
			slog.Warn("dropping report; a report is already pending", "report", report, "socket", m.fpath)
		}
		writeOK(conn, "")
		return

	case "check-ancestor":
		if len(args) != 3 {
			writeErr(conn, "invalid arguments")
			return
		}
		ppid, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			writeErr(conn, "invalid arguments")
			return
		}
		pid, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			writeErr(conn, "invalid arguments")
			return
		}
		// pidMap requires no lock because it is only accessed by the command
		// handler and it handles one socket at a time, sequentially.
		if m.pidMap[int(ppid)] || m.pidMap[int(pid)] {
			m.pidMap[int(ppid)] = true
			m.pidMap[int(pid)] = true
			writeOK(conn, "")
			return
		}
		writeErr(conn, "not an ancestor")
		return

	default:
		writeErr(conn, "unknown command")
		slog.Warn("invalid/unrecognized input command", "cmd", cmd, "args", args)
		return
	}
}

// writeOK sends a success reply terminated by a newline, optionally carrying a
// payload: "OK\n" or "OK <payload>\n".
func writeOK(conn net.Conn, payload string) {
	if payload == "" {
		fmt.Fprint(conn, "OK\n")
		return
	}
	fmt.Fprintf(conn, "OK %s\n", payload)
}

// writeErr sends an error reply: "ERR <message>\n".
func writeErr(conn net.Conn, msg string) {
	fmt.Fprintf(conn, "ERR %s\n", msg)
}

func (m *Mutex) sendCmd(ctx context.Context, cmd string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", m.fpath)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// DialContext only bounds the dial. Honor ctx for the Write/Read phase too:
	// when ctx is canceled or its deadline passes, force any blocked I/O to
	// return immediately so a wedged peer cannot make sendCmd hang forever.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	if _, err := conn.Write([]byte(cmd)); err != nil {
		return "", ctxErr(ctx, err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", ctxErr(ctx, err)
	}
	return parseReply(reply)
}

// parseReply interprets a server reply framed as "OK\n", "OK <payload>\n", or
// "ERR <message>\n". It returns the payload (empty for a bare OK) on success, or
// an error carrying the message for an ERR reply. Anything else — an empty,
// truncated, or otherwise malformed reply — is treated as an error, never as
// success, so a dead or half-open peer is not mistaken for a completed command.
func parseReply(reply string) (string, error) {
	line := strings.TrimRight(reply, "\r\n")
	switch {
	case line == "OK":
		return "", nil
	case strings.HasPrefix(line, "OK "):
		return line[len("OK "):], nil
	case line == "ERR":
		return "", errors.New("lock owner reported an error")
	case strings.HasPrefix(line, "ERR "):
		return "", errors.New(line[len("ERR "):])
	default:
		return "", fmt.Errorf("malformed response from lock owner: %q", reply)
	}
}

// ctxErr maps an I/O error to the context's cause when ctx is done. When the
// AfterFunc above unblocks a stalled connection, Write/Read report a deadline
// error; this restores the real reason (cancellation or deadline) for callers.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}

// lockIsFree reports whether an error from Owner means the lock is genuinely
// unheld: either the socket file does not exist (ENOENT) or nothing is
// listening on it (ECONNREFUSED), which is what a dead owner leaves behind. Any
// other error (timeout, connection reset, a garbled or non-numeric reply) is
// ambiguous — a live owner may still hold the lock — so it must not be treated
// as free, otherwise TryLock would steal a lock that is still held.
func lockIsFree(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// Owner returns the PID of the process currently holding the lock.
func (m *Mutex) Owner(ctx context.Context) (int, error) {
	response, err := m.sendCmd(ctx, "getpid")
	if err != nil {
		return -1, err
	}
	pid, err := strconv.ParseInt(response, 10, 64)
	if err != nil {
		return -1, err
	}
	return int(pid), nil
}

// shutdown sends a shutdown request addressed to owner, the PID of the process
// expected to hold the lock. A process holding the socket whose PID differs
// (a newcomer that acquired the lock after owner exited) ignores the request.
func (m *Mutex) shutdown(ctx context.Context, owner int) error {
	_, err := m.sendCmd(ctx, fmt.Sprintf("shutdown %d", owner))
	return err
}

// CheckAncestor verifies if the lock owner is a parent or ancestor of the current
// process. It returns nil if the check passes, otherwise an error. The lock owner
// tracks PIDs of processes that pass this check to support deeply nested descendants.
func (m *Mutex) CheckAncestor(ctx context.Context) error {
	pid := os.Getpid()
	cmd := fmt.Sprintf("check-ancestor %d %d", m.ppid, pid)
	_, err := m.sendCmd(ctx, cmd)
	return err
}

// TryLock attempts to acquire the lock without waiting. On success it returns a
// non-nil unlock function, the current process's PID as owner, and a nil error.
// On failure it returns a nil unlock function, the PID of the process currently
// holding the lock (or -1 if that could not be determined), and an error. The
// error is os.ErrInvalid when the lock is already held by the current process.
func (m *Mutex) TryLock(ctx context.Context) (unlockf func(), owner int, status error) {
	if pid, err := m.Owner(ctx); err == nil {
		if pid == os.Getpid() {
			return nil, pid, os.ErrInvalid
		}
		return nil, pid, fmt.Errorf("locked by another process %d", pid)
	} else if !lockIsFree(err) {
		// The owner's status is ambiguous (timeout, reset, garbled reply). We
		// cannot confirm the lock is free, so refuse rather than risk stealing
		// a lock that is still held.
		return nil, -1, fmt.Errorf("could not determine lock owner: %w", err)
	}

	tmpPath, err := m.startServer(ctx)
	if err != nil {
		return nil, -1, err
	}
	defer func() {
		if status != nil {
			m.stopServer(os.ErrClosed)
		}
	}()

	if err := os.Rename(tmpPath, m.fpath); err != nil {
		return nil, -1, fmt.Errorf("could not publish lock socket: %w", err)
	}

	pid, err := m.Owner(ctx)
	if err != nil {
		return nil, -1, err
	}
	if pid != os.Getpid() {
		return nil, pid, fmt.Errorf("lock won by another process")
	}
	return func() { m.stopServer(ErrUnlocked) }, pid, nil
}

// Lock acquires the lock, waiting until it is available or the context expires.
// It only waits while the lock is held by another live process; it does not
// retry indefinitely on every failure. Specifically, Lock returns immediately,
// without waiting, when:
//
//   - this Mutex already holds the lock (os.ErrInvalid); or
//   - the current owner cannot be determined (a timeout, connection reset, or
//     garbled reply). An indeterminate owner signals that something is wrong, so
//     Lock fails fast rather than masking the problem by retrying.
//
// If shutdown is true, it asks the incumbent owner (the one holding the lock at
// the first failed attempt) to shut down gracefully so that this process can
// take over. The incumbent may be a different Mutex held by another goroutine in
// the current process, which supports in-process restart; Lock never asks itself
// (this Mutex) to shut down. The request is sent at most once; a process that
// acquires the lock later while we wait is not asked to stand down. Note that
// shutdown is advisory and reaches whichever process holds the socket at the
// time. It returns an unlock function and nil on success, or an error on
// failure.
func (m *Mutex) Lock(ctx context.Context, shutdown bool) (unlockf func(), status error) {
	for {
		closef, owner, err := m.TryLock(ctx)
		if err == nil {
			return closef, nil
		}
		// If this Mutex itself already holds the lock, waiting is pointless and a
		// shutdown request would target ourselves; fail fast. Likewise fail fast
		// when the owner cannot be determined.
		if m.listener.Load() != nil || owner <= 0 {
			return nil, err
		}
		// A distinct incumbent holds the lock (possibly another instance in this
		// same process). Ask it to stand down, exactly once.
		if shutdown {
			shutdown = false
			if err := m.shutdown(ctx, owner); err != nil {
				if !lockIsFree(err) {
					return nil, err
				}
				// The owner already exited, so the lock is likely free now:
				// retry acquisition immediately instead of waiting the backoff.
				continue
			}
		}
		timeout := 50*time.Millisecond + time.Duration(rand.Intn(100))*time.Millisecond
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-time.After(timeout):
		}
	}
}

// WithLock returns a context that is canceled when the input context is canceled,
// the mutex is unlocked (ErrUnlocked), or a shutdown request is received
// (ErrShutdown). It returns os.ErrInvalid if the mutex is not held by the
// current process. The mutex is not automatically unlocked when the context is
// canceled.
func WithLock(ctx context.Context, m *Mutex) (context.Context, error) {
	pid, err := m.Owner(ctx)
	if err != nil {
		return nil, err
	}
	if pid != os.Getpid() {
		return nil, fmt.Errorf("not locked by this process")
	}

	nctx, ncancel := context.WithCancelCause(ctx)
	m.mu.Lock()
	m.derivedCancels = append(m.derivedCancels, ncancel)
	m.mu.Unlock()
	return nctx, nil
}

// WaitForReport blocks until a status report is received via the Unix domain
// socket or the context is canceled. It is called by the lock owner to wait for
// the initialization status reported by a direct child (see Report), and returns
// that status, nil on a success report, or the context's cause on cancellation.
func WaitForReport(ctx context.Context, m *Mutex) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case report := <-m.reportCh:
		if len(report) == 0 {
			return nil
		}
		return errors.New(report)
	}
}

// Report sends a status message to the lock owner. A nil status indicates
// success.
//
// Reporting is restricted to a direct child of the lock owner: the report is
// addressed to this process's parent PID (os.Getppid, captured by New), and the
// owner accepts it only when that PID matches its own. In other words, the
// caller must be a direct child and the lock owner must be the caller's parent.
// This holds only while that parent is still alive; if the parent has exited (so
// the caller has been reparented, e.g. after a double fork) or the owner is a
// more distant ancestor rather than the direct parent, the report is rejected
// and an error is returned. Use WaitForReport in the owner to receive it.
//
// Only one report can be sent per mutex; subsequent calls are no-ops. It returns
// an error if the report cannot be sent.
func Report(ctx context.Context, m *Mutex, status error) error {
	if m.reported {
		return nil
	}
	cmd := fmt.Sprintf("report %d", m.ppid)
	if status != nil {
		cmd = fmt.Sprintf("report %d %v", m.ppid, status)
	}
	if _, err := m.sendCmd(ctx, cmd); err != nil {
		if !strings.Contains(err.Error(), ": connection refused") {
			slog.Warn("could not send status report", "status", status, "err", err, "socket", m.fpath)
		}
		return err
	}
	m.reported = true
	return nil
}
