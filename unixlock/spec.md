# unixlock — Normative Specification

Status: normative. Version: draft 1.

This document specifies the intended behavior of the `unixlock` package: a
cooperative, single-host, inter-process mutual-exclusion lock built on a Unix
domain socket. It is written against the package's *intended purpose* and is the
authority when the implementation and this document disagree; such disagreements
are defects in the implementation.

## 1. Terminology

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHALL NOT**,
**SHOULD**, **SHOULD NOT**, **MAY**, and **OPTIONAL** are to be interpreted as
described in RFC 2119.

- **Lock path** — the filesystem path supplied to `New`. The listening Unix
  domain socket bound at this path *is* the lock.
- **Owner** — the process whose server is currently bound to, and answering on,
  the lock path.
- **Mutex** — an in-process handle (`*Mutex`) referring to a lock path.
- **Server** — the goroutine(s) in the owner that accept connections on the lock
  path and answer commands.
- **Client** — code (in any process) that connects to the lock path to issue a
  command.
- **Same-host** — all cooperating processes run on one operating-system kernel
  and can reach the lock path on a shared filesystem.

## 2. Purpose and scope

2.1. The package **SHALL** provide advisory, cooperative mutual exclusion among
same-host processes, keyed by a lock path.

2.2. The lock **SHALL** be *cooperative*: it constrains only processes that use
this package against the same lock path. It **MUST NOT** be relied upon to
exclude arbitrary or hostile programs.

2.3. Beyond exclusion, the package **SHALL** support: querying the owner's PID;
requesting the owner to shut down gracefully; verifying that the owner is an
ancestor of the caller; and a direct child reporting a one-time startup status to
its owning parent.

2.4. The package is **NOT** a distributed lock. Cross-host coordination is out of
scope.

## 3. Lock model

3.1. **Liveness is intrinsic.** Ownership **SHALL** be defined by a live,
answering server bound to the lock path. If the owner process terminates for any
reason, the lock **MUST** become available without any cleanup action by other
processes.

3.2. A client **MUST** determine availability by attempting to communicate with
the lock path, not by the mere existence of the socket file. A stale socket file
left by a dead owner **MUST NOT** be interpreted as a held lock.

3.3. Acquisition **MUST** be atomic with respect to concurrent contenders on the
same host: at most one process **SHALL** believe it holds the lock at any instant
for a given lock path. When multiple processes race, exactly one **MUST** win and
the others **MUST** observe failure.

3.4. Acquisition **SHOULD** be implemented so that a partially-completed attempt
never leaves a lock that no live process owns, and never leaves an orphaned
auxiliary file on success or failure (see §9).

3.5. A held lock **SHALL** remain held until the owner explicitly releases it
(§4.4) or the owner process terminates.

## 4. Core operations

### 4.1 New

4.1.1. `New(path)` **SHALL** return a `*Mutex` bound to `path`. It **MUST NOT**
create the socket or acquire the lock.

4.1.2. `New` **SHALL** capture the creating process's parent PID
(`os.Getppid`) for later use by `Report` (§7) and `CheckAncestor` (§6).

### 4.2 TryLock (non-blocking acquisition)

4.2.1. `TryLock` **SHALL** attempt to acquire the lock exactly once, without
waiting, and return promptly.

4.2.2. On success it **SHALL** return a non-nil release function and a nil error.
On failure it **SHALL** return a nil release function and a non-nil error.

4.2.3. `TryLock` **SHALL** report the owner's PID: on success, the current
process's PID; on failure because another process holds the lock, that holder's
PID; and a sentinel non-PID value (e.g. `-1`) when the owner could not be
determined.

4.2.4. If the lock is already held by the calling process, `TryLock` **SHALL**
fail and **SHOULD** return a distinguishable error indicating an invalid,
self-conflicting request.

4.2.5. `TryLock` **MUST NOT** acquire (steal) the lock unless it has positively
determined the lock is free — i.e. that no live owner is answering. An error that
merely indicates the owner's status could not be determined (timeout, reset,
malformed reply) **MUST NOT** be treated as "free."

4.2.6. If publishing the newly created server at the lock path fails, `TryLock`
**MUST** report that failure and **MUST NOT** report success.

### 4.3 Lock (blocking acquisition)

4.3.1. `Lock(ctx, shutdown)` **SHALL** attempt acquisition and, while the lock is
held by another live process, **SHALL** wait and retry until it acquires the
lock or `ctx` is done.

4.3.2. `Lock` **MUST** stop waiting and return promptly when the calling process
already holds the lock, or when the owner cannot be determined. An indeterminate
owner is treated as an error condition (fail-fast), **NOT** as a transient state
to retry.

4.3.3. Retries **SHOULD** use a randomized backoff to reduce contention among
competing waiters.

4.3.4. When `ctx` is canceled or its deadline passes, `Lock` **SHALL** return the
context's cause.

4.3.5. When `shutdown` is true, `Lock` **SHALL** request that the incumbent owner
— the process holding the lock at the first failed attempt — shut down gracefully
(§5). This request **SHALL** be sent at most once. A process that acquires the
lock later, while `Lock` is waiting, **MUST NOT** be asked to shut down.

4.3.6. The shutdown request **SHALL** be addressed to the observed incumbent's
PID, so that a different process which has since taken the lock ignores it
(§5.3).

4.3.7. If the shutdown request fails because the incumbent has already exited,
`Lock` **MUST** treat this as non-fatal and continue attempting acquisition
(the lock is now likely free), retrying without unnecessary delay.

### 4.4 Release and Close

4.4.1. The release function returned by a successful `TryLock`/`Lock` **SHALL**
relinquish the lock, stop the server, and cause the lock path to stop answering.

4.4.2. `Close` **SHALL** release the lock if held and **SHALL** cancel every
context derived via `WithLock` (§4.6), with a cause indicating closure.

4.4.3. Release/Close **SHALL** wait for the owner's server goroutines to
terminate before returning, so that no background goroutine outlives it.

4.4.4. Release **SHALL** be idempotent-safe against a subsequent `Close`: calling
both **MUST NOT** panic or corrupt state.

### 4.5 Owner

4.5.1. `Owner(ctx)` **SHALL** return the PID reported by the current owner, or an
error if no live owner is answering or the owner could not be determined.

4.5.2. `Owner` **MUST** honor `ctx`: cancellation or deadline **SHALL** abort the
call promptly with the context's cause, even if a peer is unresponsive.

### 4.6 WithLock

4.6.1. `WithLock(ctx, m)` **SHALL** return a child context that is canceled when
any of the following occurs: the parent `ctx` is canceled; the lock is released
(cause: unlocked); or a shutdown request is received by the owner (cause:
shutdown).

4.6.2. `WithLock` **SHALL** fail if the current process is not the owner.

4.6.3. `WithLock` **MUST NOT** unlock the mutex when the derived context is
canceled; releasing remains the caller's responsibility.

4.6.4. Deriving contexts, canceling them on shutdown, and releasing concurrently
**MUST** be free of data races.

## 5. Shutdown request

5.1. A shutdown request is an advisory signal asking the owner to wind down so
another process can take over. It **SHALL** cause the owner to cancel all
contexts derived via `WithLock` with a shutdown cause.

5.2. Shutdown is advisory: the owner is not forced to exit. Whether and when the
owner releases the lock in response is determined by the owner's own code
reacting to the canceled context. A requester **MUST NOT** assume the lock is
free merely because a shutdown request succeeded.

5.3. A shutdown request **MAY** be addressed to a specific owner PID. An owner
whose PID does not match the addressed PID **MUST** ignore the request (perform
no cancellation) and **SHOULD** acknowledge it as accepted, so the requester is
not misled into treating the mismatch as a failure.

## 6. Ancestor verification

6.1. `CheckAncestor(ctx)` **SHALL** succeed if and only if the owner is an
ancestor (parent, grandparent, …) of the calling process, and fail otherwise.

6.2. To support deeply nested descendants, the owner **SHALL** be permitted to
remember callers that have passed the check, so that descendants of an
already-verified process also pass.

6.3. The verification **SHOULD NOT** be spoofable by an unrelated process: an
implementation **SHOULD** verify the connecting peer's identity via a
kernel-provided credential rather than trusting PIDs supplied in the request
(§10).

## 7. Startup status reporting

7.1. `Report(ctx, m, status)` **SHALL** deliver a one-time startup status from a
**direct child** of the owner to the owner. A nil `status` denotes success; a
non-nil `status` denotes failure and conveys a message.

7.2. Reporting is restricted to a direct child: the report **SHALL** be addressed
to the caller's parent PID, and the owner **SHALL** accept it only when that PID
is the owner's own. Consequently the caller **MUST** be a direct child and the
owner **MUST** be the caller's live parent. If the parent has exited (so the
caller was reparented) or the owner is a more distant ancestor, the report
**SHALL** be rejected.

7.3. At most one report **SHALL** be delivered per mutex; subsequent `Report`
calls **SHALL** be no-ops that return success.

7.4. `WaitForReport(ctx, m)` **SHALL** be called by the owner to block until a
report arrives or `ctx` is done. It **SHALL** return: nil for a success report;
an error carrying the message for a failure report; or the context cause on
cancellation.

7.5. Delivery of a report to the owner **MUST NOT** be able to block the owner's
server. If a report is already pending and undelivered, a further report **MAY**
be dropped; the first report is the one that matters.

## 8. Wire protocol

8.1. The lock path **SHALL** speak a line-oriented request/response protocol over
a per-connection exchange: the client connects, sends one request, reads one
reply, and the connection is closed.

8.2. A request **SHALL** be a single line of whitespace-separated fields. An
implementation **SHOULD** frame requests by a terminating newline and **MUST NOT**
misinterpret or silently truncate a request that does not fit an implementation's
internal buffer in a single read.

8.3. The defined commands are:

| Command | Fields | Meaning |
|---|---|---|
| `getpid` | — | return the owner's PID |
| `shutdown` | `[targetPID]` | request graceful shutdown, optionally addressed |
| `check-ancestor` | `ppid pid` | verify owner is an ancestor of the caller |
| `report` | `targetPID [message…]` | deliver a startup status to the owner |

8.4. A reply **SHALL** be exactly one newline-terminated line and **SHALL** be
one of:

- `OK` — success with no payload;
- `OK <payload>` — success carrying a payload (e.g. a PID);
- `ERR <message>` — failure, with a human-readable message.

8.5. A client **MUST** treat a well-formed `OK` reply as success and a well-formed
`ERR` reply as failure. A client **MUST** treat an empty, truncated, or otherwise
malformed reply as a failure — **never** as success. Absence of bytes is not a
success signal.

8.6. An unrecognized or malformed request **SHALL** be answered with `ERR`.

8.7. The protocol is unversioned in this draft. Implementations **SHOULD** treat
unknown commands as errors (§8.6) to leave room for future extension.

## 9. Resource lifecycle

9.1. Acquisition **SHOULD** create its server on a private, per-attempt path and
publish it into place atomically, so a concurrent contender never observes a
half-initialized lock.

9.2. On a failed acquisition, an implementation **MUST NOT** leave behind an
orphaned auxiliary socket file.

9.3. It is a known and accepted consequence of the liveness model (§3) that the
socket file at the lock path **MAY** persist after the owner dies; this stale file
**MUST NOT** be treated as a held lock (§3.2) and **MAY** be replaced by the next
acquirer.

9.4. `New` **MAY** validate that the lock path is usable (e.g. that the parent
directory exists and the path length is within the platform's socket-address
limit) and **SHOULD** surface such problems as clear errors rather than opaque
system errors.

## 10. Concurrency and safety

10.1. All exported operations on a `*Mutex` **MUST** be safe for concurrent use
by multiple goroutines.

10.2. The owner's server **MUST NOT** be stallable by a single client: a slow,
silent, or non-reading client **MUST NOT** prevent the server from handling other
requests. Implementations **SHOULD** bound per-connection service time.

10.3. Every client operation that performs I/O **MUST** honor its `ctx` for the
entire exchange (connect, write, and read), not merely the connection phase.

## 11. Security considerations

11.1. The lock is advisory and assumes cooperating, mutually-trusting processes,
normally running under the same user on one host. It provides no protection
against a hostile local process that chooses not to cooperate.

11.2. Because request-carried PIDs are attacker-controlled data, any decision that
must be trustworthy (ancestor verification §6, restricting who may request
shutdown or report) **SHOULD** be based on the kernel-verified identity of the
connecting peer (e.g. `SO_PEERCRED`: peer UID/PID) rather than on values in the
request.

11.3. An implementation **SHOULD** restrict the socket's filesystem permissions
and **SHOULD** document the expected ownership of the containing directory, so
that only intended processes can reach the lock path.

11.4. Recycled PIDs are a hazard: any memoized trust keyed on PID (§6.2)
**SHOULD** be bounded and **SHOULD NOT** grant trust indefinitely.

## 12. Conformance

An implementation conforms to this specification if it satisfies every **MUST**
and **MUST NOT** requirement herein. Requirements marked **SHOULD** are strong
recommendations; deviations **SHOULD** be documented. Requirements marked **MAY**
are optional.
