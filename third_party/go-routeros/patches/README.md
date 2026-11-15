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

- `run.go`;
- the reader half of `proto/io_context.go`.

No protocol encoding, login behavior, RouterOS sentence, route command, reply
parsing, `Client.Close`, `Listen*`, or writer-context production code differs
from PR #32.

### `run.go`: required by the plugin's Async command path

The plugin sends every route operation through `RunArgsContext` on one Async
connection. PR #32 acquires the sentence writer before checking whether the
Async tag map still exists. If the Async loop ended in that interval, it
returns without `EndSentence`, which is the operation that unlocks the writer.
One command can therefore leave every concurrent/future command blocked.

The local change checks and registers the tag first, releases the client mutex,
and only then acquires the sentence writer. This is safe for RouterOS because
tag registration is entirely local. The bytes on the native TCP API remain in
the same order:

1. command and argument words;
2. `.tag=<generated tag>`;
3. the empty word written by `EndSentence`.

The writer still serializes complete sentences, and RouterOS sees no new or
changed command.

### `proto/io_context.go`: required by the plugin's Async TCP reader

The plugin ends a failed connection generation by cancelling its
`AsyncContext` and closing the TCP connection. Those events can race, and
cancellation can occur before the reader starts its next `Read`.

PR #32 uses a best-effort non-blocking send on an unbuffered channel:

- cancellation is lost when no `Read` is currently selecting on the channel;
- Cancel can pass its atomic check immediately before Close closes the channel,
  then panic while attempting to send on that closed channel.

Only `ctxReader` is changed to `sync.Once` plus `close(done)`. Cancellation is
therefore persistent and idempotent. This matches the plugin lifecycle: an
ended connection generation is never reused; it is replaced by reconnect.
`ctxWriter` is deliberately left exactly at PR #32 because the project does
not call `Writer.Cancel`.

This is local goroutine/channel behavior only. It sends no RouterOS API word,
does not issue `/cancel`, and does not change TCP framing.

## Explicitly out of scope

- The project currently uses the native TCP API. No HTTP code is present or
  changed.
- TLS is supported by the parent plugin but is not a reason for either local
  patch; no TLS handshake, certificate, or transport behavior is changed.
- The plugin does not use go-routeros `Listen*`; that code is byte-for-byte
  PR #32.
- `Client.Close`, protocol reader/writer encoding, login, replies, and all
  route business logic are byte-for-byte PR #32.

## Non-production differences

- `client_test.go` adds the blocked-writer/Close regression test required by
  the retained `run.go` change.
- `proto/io_context_test.go` proves cancellation before the first/next Read is
  not lost; this test deterministically blocks on PR #32.
- `README.md` differs only by removal of trailing whitespace.
- `.github/workflows/go.yml`, `.gitignore`, and the three `examples/` programs
  were not copied because this is a dependency source snapshot, not a fork
  development checkout. Their omission cannot affect the built module.
- `PATCHES.md` and `patches/` are local provenance documents only.

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

The retained paths are covered by race tests repeated 100 times, the complete
parent and dependency race suites, and a real CRS504 native-TCP test using one
Async connection for 64 concurrent adds and 64 concurrent removes. All routes
were active and hardware-offloaded, cleanup left zero routes, and the protected
route remained unchanged.

Before upgrading upstream, compare its source with this patch. Remove each hunk
that upstream has merged, rerun the race/failure tests, and delete the root
`replace` after no local hunk remains.
