# tgrep-go

A Go port of [microsoft/tgrep](https://github.com/microsoft/tgrep) — trigram-indexed
grep with a client/server architecture for fast regex search in large codebases.

The index format is **byte-compatible** with the Rust implementation's, so either
tool can read the other's index, and the two can be diffed against each other
rather than each being trusted on its own tests.

```sh
go build -o tgrep ./cmd/tgrep

./tgrep index .          # build the index
./tgrep serve .          # serve searches over TCP, watching for changes
./tgrep "fn main" .      # search
./tgrep status .         # what the index holds
```

## Why a port

The reference implementation is Rust, and linking it into a Go program means a
C-ABI shim plus cross-compiling Rust for every release target. A native Go
package can be imported directly — no subprocess, no second binary, and no
second walker to disagree with the host program's own.

## What is here

| Package | |
|---|---|
| `trigram` | trigram hashing, merged-case extraction, location and Bloom masks |
| `ondisk` | the binary formats: `lookup.bin`, `index.bin`, `files.bin` |
| `meta` | `meta.json`, `filestamps.json`, blake3 content identities |
| `pathindex` | the `files-extra.bin` sidecar of listable-but-unsearchable paths |
| `reader` | mmap'd index with binary-searched lookups |
| `index` | streaming writer and the external merge sorter |
| `query` | regex → trigram plan, plan execution, PCRE relaxation |
| `walker` | repository walk with the gitignore engine |
| `gitignore` | gitignore(5) pattern matching |
| `matching` | line indexing, the matcher, context windows |
| `search` | candidate resolution and parallel file search |
| `live` | in-memory overlay and the file watcher |
| `hybrid` | the merge of on-disk index and overlay |
| `output` | ripgrep-compatible output formats |
| `server` | JSON-RPC 2.0 over newline-delimited TCP |

## Verification

`compat/` holds differential tests that read bytes written by the reference
implementation and drive its binary, rather than only checking this port against
its own assumptions.

- **Index bytes.** A Go-built index is byte-identical to a Rust-built one over
  the same file list: `lookup.bin`, `index.bin`, `files.bin`, `files-extra.bin`.
- **Postings rebuilt from source.** Every trigram in the reference index is
  re-derived from the fixture files and compared, file ids and both masks
  included.
- **Walk admission.** Set parity on six real checkouts, up to 3,495 files.
- **Search results.** Identical matches down to the byte column, on synthetic
  corpora and on whole repositories — 8,317 matches for one pattern on `loop`.
- **Planner soundness.** 400 generated patterns assert that every file which
  really matches survives into the candidate set.

Run the standalone harness against any repository:

```sh
go run ./cmd/tgrep-diff --repo ~/src/project
```

It builds both indexes, compares admitted paths, compares `lookup.bin` byte for
byte, and runs a pattern corpus through both binaries. Exit status is non-zero
on any disagreement.

Six repositories currently pass all 25 checks — `tgrep`, `odin`, `oboe.chat`,
`drover`, `loop` (1,836 paths) and `arc` (3,495 paths, `node_modules` included).

The harness earns its keep: it found a defect on a real checkout that no
hand-written case had reached. Searching `context|Context` over
`ContextContext` is two matches whose spans abut, and a span merge written with
`<=` instead of `<` collapsed them into one — reporting a single match where
ripgrep and the reference both report two. It surfaced in TypeScript's own
compiler output, in the identifier
`shouldEnterSuppressNewDiagnosticsContextContext`.

A repository that vendors minified or generated code needs care: one 174KB
source-map line repeated per match makes `return --vimgrep` produce **5.3 GB**
of output from the reference implementation alone. The harness streams output
and caps it at 200,000 rows, reporting the comparison as skipped rather than
truncating it silently.

## Deliberate differences

**Walk order is deterministic.** The reference's parallel walker is not: five
runs over one unchanged 320-file tree produced three different file orders, and
since file ids are positional, its `files.bin` and `index.bin` differ run to run.
This port sorts every directory, so the same tree always produces the same index
bytes. (`lookup.bin` is order-independent and matches either way — which is why
the harness compares that one.)

**Two gitignore fixes.** The reference disagrees with git and with ripgrep in two
places, and in both it hides a file the user expects to be searchable:

- A nested `.gitignore` negation (`!*.tmp` under a root `*.tmp`) — `git
  check-ignore -v` attributes the file to the negation and `git status` lists it.
- `.ignore` outranking a `.gitignore` beside it — `rg --files` lists the
  re-included file.

This port follows git and ripgrep. `compat/walker_test.go` records both, asserts
this port's behaviour, and logs the reference's, so a future upstream fix simply
turns them into two more parity cases.

**No backreferences or lookaround.** Go's `regexp` is RE2: linear time, but it
cannot express them, and there is no `fancy_regex` equivalent in the standard
library. Such a pattern is rejected with a clear message rather than compiled
into something that means something else. Index-side relaxation for `-P`
patterns is still implemented, so adding an engine later touches one file.

## Performance

Measured on `loop` (1,836 files, macOS arm64), median of 5 cold-process runs:

| pattern | ripgrep | tgrep (Rust) | tgrep-go |
|---|---:|---:|---:|
| `handleRequest` | 61ms | 26ms | 24ms |
| `error` | 48ms | 47ms | 35ms |
| `^import` | 44ms | 78ms | 44ms |

Index build over the same tree: Rust 0.6s, Go 0.9s.

Read these numbers carefully. 1,836 files is small — the reference's own
benchmarks use repositories of 100k–500k files, where the index has far more to
save. On a tree this size a common pattern returning thousands of matches spends
more time delivering them than the index saves finding them, which is why
`^import` is a tie and why the reference itself loses to ripgrep there. The
index earns its keep on rare patterns and large trees.

## License

MIT, matching the upstream project.
