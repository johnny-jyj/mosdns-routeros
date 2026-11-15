# Local go-routeros patch set

This module is based on `github.com/go-routeros/routeros/v3` v3.0.1
(`edd18cda189d1bb0302a2d7a8089357b7ea1f26c`) plus upstream PR #32 commit
`6e4fd2ed57ae34eac00e041972c4d1b9a832ff2b`.

PR #32 fixes:

- registering async reply tags after sending, which can drop fast replies;
- cancelling the shared reader when one `RunArgsContext` is cancelled.

The local follow-up in `run.go` registers an async command's tag before taking
the writer sentence lock. PR #32 takes the writer lock first, then returns
without `EndSentence` if the async loop has ended, permanently leaking that
lock. The RouterOS plugin uses this `RunArgsContext` path.

`proto/io_context.go` also uses `sync.Once` to close its cancellation channel.
The upstream non-blocking send can be lost before a read starts, while racing
`Cancel` with `Close` can send on a channel as it is being closed.

No other production source differs from PR #32. In particular, the plugin does
not use the `Listen*` API, so that path is kept exactly at PR #32 rather than
carrying unrelated local lock changes.

The complete machine-applicable delta and a visual summary are archived in
`patches/0001-mosdns-run-cancel-lifecycle.patch` and `patches/README.md`.

## Mandatory upgrade checklist

The root `go.mod` has a local `replace`, so changing its `require` version alone
does **not** update the code used by the build. For every go-routeros upgrade:

1. Check the upstream tag/HEAD and PR #32 status.
2. Diff upstream against this directory and classify every local patch above
   as merged, still required, or obsolete.
3. Rebase/copy only the patches still required; never overwrite this directory
   with upstream without that comparison.
4. Run the module race tests and the parent project's RouterOS failure,
   concurrency, benchmark, and real-device E2E tests.
5. Prefer a remotely pinned fork pseudo-version once these patches are
   published. Delete the local `replace` when upstream contains every fix.
