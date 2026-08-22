// Copyright (c) 2025 Visvasity LLC

package unixlock

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBasicLocking(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	ctx := context.Background()
	lock := New(lockPath)

	// Try to acquire the lock
	closef, _, err := lock.TryLock(ctx)
	if err != nil {
		t.Fatalf("first TryLock failed: %v", err)
	}
	if closef == nil {
		t.Fatal("first TryLock returned nil close function")
	}

	// Second lock in same process should fail
	lock2 := New(lockPath)
	if _, _, err := lock2.TryLock(ctx); err == nil {
		t.Fatal("expected second TryLock to fail, but it succeeded")
	}

	// Verify GetOwnerPid returns our PID
	pid, err := lock.Owner(ctx)
	if err != nil {
		t.Fatalf("GetOwnerPid failed: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("expected owner pid %d, got %d", os.Getpid(), pid)
	}

	// Release the lock
	closef()

	// Wait a moment for cleanup
	time.Sleep(100 * time.Millisecond)

	// Now second lock should succeed
	closef2, _, err := lock2.TryLock(ctx)
	if err != nil {
		t.Fatalf("second TryLock after unlock failed: %v", err)
	}
	closef2()
}

// TestEmptyCommand ensures a whitespace-only request does not crash the lock
// server's accept goroutine (which would take down the owning process).
func TestEmptyCommand(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	ctx := context.Background()
	lock := New(lockPath)

	closef, _, err := lock.TryLock(ctx)
	if err != nil {
		t.Fatalf("TryLock failed: %v", err)
	}
	defer closef()

	// Send a whitespace-only payload directly to the socket.
	conn, err := net.Dial("unix", lockPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	if _, err := conn.Write([]byte("   ")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	conn.Close()

	// The server must still be alive and answering.
	pid, err := lock.Owner(ctx)
	if err != nil {
		t.Fatalf("Owner failed after empty command (server likely crashed): %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("expected owner pid %d, got %d", os.Getpid(), pid)
	}
}

// TestWithLockShutdownRace exercises WithLock (which appends to
// m.derivedCancels) concurrently with the "shutdown" command handler (which
// ranges over the same slice on the accept goroutine). Run under -race to
// detect the unsynchronized access.
func TestWithLockShutdownRace(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	ctx := context.Background()
	lock := New(lockPath)

	closef, _, err := lock.TryLock(ctx)
	if err != nil {
		t.Fatalf("TryLock failed: %v", err)
	}
	defer closef()

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: repeatedly append cancel funcs via WithLock.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := WithLock(ctx, lock); err != nil {
				t.Errorf("WithLock failed: %v", err)
				return
			}
		}
	}()

	// Reader: repeatedly trigger the shutdown handler, which ranges over
	// derivedCancels on the accept goroutine.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			conn, err := net.Dial("unix", lockPath)
			if err != nil {
				continue
			}
			conn.Write([]byte("shutdown"))
			conn.Close()
		}
	}()

	wg.Wait()
}

// TestReportNoBlock ensures a second report does not wedge the lock server's
// accept goroutine (the buffered channel holds only one), and that the first
// report is still delivered to WaitForReport.
func TestReportNoBlock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	ctx := context.Background()
	lock := New(lockPath)

	closef, _, err := lock.TryLock(ctx)
	if err != nil {
		t.Fatalf("TryLock failed: %v", err)
	}
	defer closef()

	// Send several reports directly. No one is draining reportCh yet, so the
	// first fills the buffer and the rest must be dropped, not block.
	ownerPid := os.Getpid()
	for i := 0; i < 5; i++ {
		conn, err := net.Dial("unix", lockPath)
		if err != nil {
			t.Fatalf("dial %d failed: %v", i, err)
		}
		fmt.Fprintf(conn, "report %d first-report", ownerPid)
		conn.Close()
	}

	// Server must still be responsive after the flood of reports.
	if pid, err := lock.Owner(ctx); err != nil || pid != ownerPid {
		t.Fatalf("server unresponsive after reports: pid=%d err=%v", pid, err)
	}

	// The first (and only buffered) report must be delivered.
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := WaitForReport(wctx, lock); err == nil {
		t.Fatal("expected the buffered non-empty report, got nil")
	} else if err.Error() != "first-report" {
		t.Fatalf("expected %q, got %q", "first-report", err.Error())
	}
}

// TestSilentClientDoesNotWedge ensures a client that connects but never writes
// cannot stall the accept goroutine indefinitely: the connection deadline lets
// the server recover and keep serving other requests.
func TestSilentClientDoesNotWedge(t *testing.T) {
	// Shorten the server-side deadline for the test and restore it after.
	orig := handleTimeout
	handleTimeout = 200 * time.Millisecond
	defer func() { handleTimeout = orig }()

	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	ctx := context.Background()
	lock := New(lockPath)

	closef, _, err := lock.TryLock(ctx)
	if err != nil {
		t.Fatalf("TryLock failed: %v", err)
	}
	defer closef()

	// Connect but never write. Without a read deadline this occupies the
	// single-threaded accept loop forever.
	silent, err := net.Dial("unix", lockPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer silent.Close()

	// The server must recover once the deadline fires and answer a real
	// request well within a bound that is generous vs. handleTimeout but far
	// below "forever".
	done := make(chan error, 1)
	go func() {
		pid, err := lock.Owner(ctx)
		if err == nil && pid != os.Getpid() {
			err = fmt.Errorf("unexpected owner pid %d", pid)
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Owner failed after silent client: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server wedged by silent client: Owner did not return")
	}
}

// TestTryLockDoesNotStealOnAmbiguousOwner ensures TryLock refuses to acquire
// when the owner's status cannot be confirmed (a live listener answers with a
// non-numeric pid), rather than treating the ambiguous error as "lock free"
// and stealing a lock that may still be held.
func TestTryLockDoesNotStealOnAmbiguousOwner(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	// A live listener at the lock path that replies with garbage to every
	// request, so Owner's ParseInt fails with a non-ECONNREFUSED error.
	addr := &net.UnixAddr{Name: lockPath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("ListenUnix failed: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			conn.Read(buf)
			conn.Write([]byte("OK not-a-pid\n"))
			conn.Close()
		}
	}()

	lock := New(lockPath)
	closef, _, err := lock.TryLock(context.Background())
	if err == nil {
		closef()
		t.Fatal("TryLock stole the lock despite an ambiguous owner reply")
	}
}

// TestSendCmdHonorsContextDeadline ensures a peer that accepts but never
// replies cannot make a request hang past the context deadline (sendCmd must
// apply ctx to the Write/Read phase, not just the dial).
func TestSendCmdHonorsContextDeadline(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	addr := &net.UnixAddr{Name: lockPath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("ListenUnix failed: %v", err)
	}
	defer l.Close()

	// Accept connections and hold them open without ever replying.
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	}()

	lock := New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := lock.Owner(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Owner to fail once the context deadline passed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Owner hung despite the context deadline")
	}
}

// TestLockShutdownOwnerAlreadyGone ensures Lock(ctx, shutdown=true) does not
// fail when the owner exits before the shutdown request is sent: the shutdown
// hits a gone socket, which must be ignored so acquisition can proceed.
func TestLockShutdownOwnerAlreadyGone(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	addr := &net.UnixAddr{Name: lockPath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("ListenUnix failed: %v", err)
	}

	// Serve exactly the initial getpid: report a foreign owner so the first
	// TryLock reports "locked by another process", but unlink the socket
	// BEFORE replying. The client cannot observe the reply until conn.Close(),
	// so by the time Lock proceeds to the shutdown request the path is gone
	// (ENOENT) — deterministically exercising the "owner already gone" path.
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 256)
		conn.Read(buf)
		l.Close() // unlink the socket path first
		fmt.Fprintf(conn, "OK %d\n", os.Getpid()+100000)
		conn.Close()
	}()

	lock := New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	closef, err := lock.Lock(ctx, true)
	if err != nil {
		t.Fatalf("Lock failed even though the owner was gone: %v", err)
	}
	closef()
}

// TestLockAsksOnlyIncumbentToShutdown ensures Lock(ctx, shutdown=true) asks only
// the incumbent owner (present at the first failed attempt) to shut down, exactly
// once: even though the owner changes while we wait, the newcomer is not asked,
// and the request is never repeated.
func TestLockAsksOnlyIncumbentToShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	addr := &net.UnixAddr{Name: lockPath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("ListenUnix failed: %v", err)
	}

	// Fake owner driven by the getpid call count: owner 1000 for the first two
	// probes, then owner 2000, then it releases (unlinks) so Lock can acquire.
	// Shutdown requests are counted; only the first owner (1000) must be asked.
	var mu sync.Mutex
	getpidCalls := 0
	shutdowns := 0
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			n, _ := conn.Read(buf)
			req := string(buf[:n])
			switch {
			case strings.HasPrefix(req, "getpid"):
				mu.Lock()
				getpidCalls++
				n := getpidCalls
				mu.Unlock()
				if n <= 2 {
					fmt.Fprintf(conn, "OK 1000\n")
				} else {
					fmt.Fprintf(conn, "OK 2000\n")
				}
				conn.Close()
				if n >= 4 {
					// Release like a real owner would: close the listener so
					// the next dial fails with ENOENT (a lockIsFree error).
					l.Close()
					return
				}
				continue
			case strings.HasPrefix(req, "shutdown"):
				mu.Lock()
				shutdowns++
				mu.Unlock()
				fmt.Fprint(conn, "OK\n")
			}
			conn.Close()
		}
	}()

	lock := New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	closef, err := lock.Lock(ctx, true)
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	closef()

	mu.Lock()
	got := shutdowns
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly 1 shutdown request (incumbent only), got %d", got)
	}
}

// TestTargetedShutdown ensures a shutdown request addressed to a different PID
// is ignored, while one addressed to the owner (or an untargeted request)
// cancels the derived context.
func TestTargetedShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "testlock.sock")

	ctx := context.Background()
	lock := New(lockPath)
	closef, _, err := lock.TryLock(ctx)
	if err != nil {
		t.Fatalf("TryLock failed: %v", err)
	}
	defer closef()

	wctx, err := WithLock(ctx, lock)
	if err != nil {
		t.Fatalf("WithLock failed: %v", err)
	}

	// sendShutdown writes a shutdown command and waits for the handler to finish
	// (signaled by EOF on read) so the outcome is observable without sleeping.
	sendShutdown := func(cmd string) {
		conn, err := net.Dial("unix", lockPath)
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		fmt.Fprint(conn, cmd)
		conn.Read(make([]byte, 256))
		conn.Close()
	}

	// A shutdown for a different pid must not cancel the derived context.
	sendShutdown(fmt.Sprintf("shutdown %d", os.Getpid()+100000))
	if wctx.Err() != nil {
		t.Fatal("derived context was canceled by a shutdown for a different pid")
	}

	// A shutdown addressed to us must cancel it.
	sendShutdown(fmt.Sprintf("shutdown %d", os.Getpid()))
	select {
	case <-wctx.Done():
		if got := context.Cause(wctx); got != ErrShutdown {
			t.Fatalf("expected ErrShutdown, got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("derived context was not canceled by a shutdown addressed to us")
	}
}

// TestTryLockRenameFailure ensures that when publishing the lock socket fails,
// TryLock reports the error rather than silently succeeding, and leaves no
// orphaned temp socket behind.
func TestTryLockRenameFailure(t *testing.T) {
	tmpDir := t.TempDir()

	// Use an existing directory as the lock path: dialing it yields
	// ECONNREFUSED (so TryLock proceeds to acquire), but os.Rename cannot
	// replace a directory, so publishing the socket fails.
	lockPath := filepath.Join(tmpDir, "iamdir")
	if err := os.Mkdir(lockPath, 0o755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	lock := New(lockPath)
	closef, _, err := lock.TryLock(context.Background())
	if err == nil {
		closef()
		t.Fatal("expected TryLock to fail when publishing the socket fails")
	}
	// The error must point at the rename/publish step, not a misleading
	// downstream "connection refused" from the follow-up owner probe.
	if !strings.Contains(err.Error(), "publish lock socket") {
		t.Fatalf("expected a publish-socket error, got: %v", err)
	}

	// No orphaned "<base>.<pid>" temp socket should remain.
	entries, rerr := os.ReadDir(tmpDir)
	if rerr != nil {
		t.Fatalf("ReadDir failed: %v", rerr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "iamdir.") {
			t.Fatalf("orphaned temp socket left behind: %s", e.Name())
		}
	}
}

// TestParseReply documents the reply framing contract: "OK\n"/"OK <payload>\n"
// is success, "ERR ...\n" is an error, and anything else — crucially an empty or
// truncated reply — is an error, never success.
func TestParseReply(t *testing.T) {
	cases := []struct {
		in      string
		payload string
		wantErr bool
	}{
		{"OK\n", "", false},
		{"OK 1234\n", "1234", false},
		{"OK", "", false},
		{"ERR bad thing\n", "", true},
		{"ERR\n", "", true},
		{"", "", true},        // empty reply must not be read as success
		{"garbage\n", "", true},
		{"1234\n", "", true},  // a bare payload (old style) is no longer success
	}
	for _, c := range cases {
		got, err := parseReply(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseReply(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if got != c.payload {
			t.Errorf("parseReply(%q): payload=%q, want %q", c.in, got, c.payload)
		}
	}
}

// TestCheckAncestorRejectsSilentPeer ensures a peer that accepts a connection
// and closes it without replying is treated as a failure, not a false success —
// the core of the empty-response-as-success problem, on the security-sensitive
// ancestor check.
func TestCheckAncestorRejectsSilentPeer(t *testing.T) {
	tmpDir := t.TempDir()

	// A real lock owned by us: CheckAncestor must pass (OK path).
	okPath := filepath.Join(tmpDir, "ok.sock")
	lock := New(okPath)
	closef, _, err := lock.TryLock(context.Background())
	if err != nil {
		t.Fatalf("TryLock failed: %v", err)
	}
	defer closef()
	if err := lock.CheckAncestor(context.Background()); err != nil {
		t.Fatalf("CheckAncestor should pass for our own lock: %v", err)
	}

	// A peer that accepts and immediately closes without replying.
	silentPath := filepath.Join(tmpDir, "silent.sock")
	addr := &net.UnixAddr{Name: silentPath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("ListenUnix failed: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	silentLock := New(silentPath)
	if err := silentLock.CheckAncestor(context.Background()); err == nil {
		t.Fatal("CheckAncestor treated a silent (no-reply) peer as success")
	}
}
