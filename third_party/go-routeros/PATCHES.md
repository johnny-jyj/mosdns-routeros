# Local go-routeros patch set

This module is based on `github.com/go-routeros/routeros/v3` v3.0.1
(`edd18cda189d1bb0302a2d7a8089357b7ea1f26c`) plus upstream PR #32 commit
`6e4fd2ed57ae34eac00e041972c4d1b9a832ff2b`.

PR #32 fixes:

- registering async reply tags after sending, which can drop fast replies;
- cancelling the shared reader when one `RunArgsContext` is cancelled.

The local follow-up in `run.go` and `listen.go` checks `c.tags` before taking
the writer sentence lock. PR #32 otherwise returns with that lock held when the
async loop has already ended, causing every later command to block forever.

`proto/io_context.go` also uses `sync.Once` to close its cancellation channel.
The upstream non-blocking send can be lost before a read starts, while racing
`Cancel` with `Close` can send on a channel as it is being closed.
