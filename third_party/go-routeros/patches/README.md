# Local delta after go-routeros PR #32

## Baseline

The vendored source contains all changes from go-routeros PR #32 commit:

`6e4fd2ed57ae34eac00e041972c4d1b9a832ff2b`

PR #32 provides both behaviors required by this plugin:

1. register async reply tags before a sentence can be sent;
2. cancelling one `RunArgsContext` unregisters only that request instead of
   cancelling the shared async reader.

## Complete additional production diff

[`0001-mosdns-run-cancel-lifecycle.patch`](./0001-mosdns-run-cancel-lifecycle.patch)
is the complete production-code delta on top of PR #32. It changes only:

- `run.go`: check/register the async tag before acquiring the sentence writer,
  so an ended async loop cannot return while leaving that writer locked;
- `proto/io_context.go`: make Cancel/Close one idempotent channel close, so
  cancellation cannot be lost or race into a send on a closed channel.

No protocol encoding, login behavior, RouterOS sentence, route command, reply
parsing, `Client.Close`, or `Listen*` production code differs from PR #32.

## Verification

Against a checkout of PR #32:

```sh
git apply --check 0001-mosdns-run-cancel-lifecycle.patch
git apply 0001-mosdns-run-cancel-lifecycle.patch
```

Against the current patched source, the reverse check must pass:

```sh
git apply --check --reverse patches/0001-mosdns-run-cancel-lifecycle.patch
```

Before upgrading upstream, compare its source with this patch. Remove each hunk
that upstream has merged, rerun the race/failure tests, and delete the root
`replace` after no local hunk remains.
