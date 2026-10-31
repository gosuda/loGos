# M8 go group and logos/sync

Normative sources: `logos-docs/SPEC.md` Sec 2.6 (RFC-206), `logos-docs/IMPLEMENTATION.md`
M8-S1 to S5. This page closes the M8 P1-S3 docs row. It describes the behavior
the tree implements; parent runs the gates below to confirm the checkout.

## What the mechanism is

State machine GoGroup (validation first, built on `context`, a mutex, and
`sync.WaitGroup`):

- States Open, Cancelled, Joined. Variables: running count, first error, limit
  semaphore tokens.
- Child failure or panic records the first error in completion order, cancels
  the group context, and moves toward Joined. At Joined, running is 0, so no
  child outlives the block; the context is cancelled; the result is the first
  error.
- `src/logos/rt/group.go`: `type Group`, `NewGroup(ctx, limit)`, `Go(f)`,
  `Wait()`, `type PanicError{Value, Stack}` with `Error()`.
- `go group (ctx)` and `go group (ctx, limit = N)` syntax with
  `go { ... }` children. `?` inside a child returns to the group. A `go group`
  statement in a function whose last result is not `error` is
  `correctness::group_needs_error_result`. `return` inside a child block is
  `correctness::return_in_group_child`. A `go` child block outside a `go group`
  body is `correctness::child_outside_group`.
- Channel operations inside child bodies select on the group context:
  `v := <-ch`, `ch <- v`, and `for v := range ch` gain a Done case. A user
  `select` without `default` gains a Done case.
- Network wake (S4): `g` carries a group slot index; `pollDesc` carries a
  nonpointer `logosCancel` field (bit 1 read, bit 2 write). On cancel the
  runtime visits registered active waiters of that slot, sets the mode bit,
  and wakes the waiter with `netpollunblock` plus `goready`, so the blocked
  `Read` returns a timeout error (`os.ErrDeadlineExceeded`) and the descriptor
  stays usable.
- `src/logos/sync`: `Locked[T]` with `WithLock(f)`, `Hot[T]` with `Load`,
  `Store`, `Update` over a CAS loop on `atomic.Pointer[T]`. Stores run the Go
  hybrid write barrier; values are GC managed with no retire list. Eject
  writes it as `github.com/gosuda/loGos/rt/sync`.

## How to enable or observe it

- Write `.lgo` with `go group (ctx, limit = N) { go { ... } }`; a bare
  `go f(args)` inside the body is a child. `go group(x)` with no block keeps
  Go meaning (a call of a function named `group`).
- Observe with `src/logos/rt/group_test.go` (synctest: first error wins,
  cancel reaches siblings, limit bounds concurrency, panic becomes
  `PanicError`, `Wait` returns after all children exit; parent requires the
  `-race` leg to report no data races),
  `test/logos/run/group_chan.lgo`, `test/logos/run/group_netpoll.lgo`
  (sibling failure releases a peer blocked in `conn.Read` within 100 ms with
  an error satisfying `os.IsTimeout`; a goroutine outside the group stays
  blocked), and `src/logos/sync/sync_test.go` (unlock after panic, concurrent
  `Hot.Update` safety, retire free growth bound, `BenchmarkWithLock` and
  `BenchmarkHotLoad` as reported numbers).
- Parent to run: `go test -race -short logos/rt -run Group`,
  the testdir group run plus `testlogos` codegen checks with `-race`, and
  `GOEXPERIMENT` scoped runtime checks for the netpoll leg.

## Budgets and defaults

- Limit is explicit per group (`limit = N`); a group with no limit runs
  unbounded up to scheduler capacity.
- Group slot allocation takes the lowest free slot from a bitmap guarded by
  `logosWaitersLock`; `Group.Wait` returns the slot only after every child
  has cleared its tag and joined, so a slot never names two live groups.
- `BenchmarkWithLock` is reported within 2 ns per op of manual
  `Lock` plus `defer Unlock` (reported, not a pass condition).
  `BenchmarkHotLoad` reports one atomic pointer load (reported, not a pass
  condition). One million updates per second add no memory (retire free).

## Known limits

- Only netpoll backed descriptors wake this way. Regular files and cgo calls
  do not.
- `go f()` in `.lgo` outside a `go group` block is
  `concurrency::unsupervised_goroutine`. Reentrant `WithLock` on the same
  receiver (direct or through same package call and callback graph) is
  `concurrency::reentrant_withlock_deadlock`. A lambda that returns or stores
  its argument is rejected by the M9 lint.
- Child panic value and stack are preserved in `PanicError`; the group still
  joins before returning.
