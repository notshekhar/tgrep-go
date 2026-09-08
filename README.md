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
| `encoding` | BOM transcoding and lossy UTF-8 repair |
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
- **At scale.** Indexing 158,639 files produces a `lookup.bin` byte-identical
  to the reference's — 688,076 trigrams, same bytes.

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

Scale finds what fixtures cannot. Indexing 159,000 files exposed a divergence
in the trigram tables that six whole-repository comparisons had missed: this
port was reading raw bytes where the reference decodes first. A UTF-16 file is
NUL-interleaved, so it was being classified as binary and skipped — silently
unfindable, with no way to tell the result from "not present". Fixing it
brought the two tables to byte-identical at that scale.

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

**Encoding follows ripgrep.** A UTF-8 or UTF-16 byte-order mark is sniffed and
the file transcoded; otherwise bytes are read as UTF-8 with invalid sequences
repaired to U+FFFD. No other detection is attempted — guessing an encoding from
content is unreliable, and ripgrep deliberately does not. Repair substitutes one
replacement per *maximal subpart*, which is the Unicode recommendation and not
the same as one per byte; `compat/encoding_test.go` pins the granularity against
counts measured from the reference.

**No backreferences or lookaround.** Go's `regexp` is RE2: linear time, but it
cannot express them, and there is no `fancy_regex` equivalent in the standard
library. Such a pattern is rejected with a clear message rather than compiled
into something that means something else. Index-side relaxation for `-P`
patterns is still implemented, so adding an engine later touches one file.

## Performance

macOS arm64, median of 5 cold-process runs. Both indexes cover the same file
set, and match counts were verified equal before timing.

**158,639 files** (`loop` indexed with `--hidden --no-ignore`):

| pattern | matches | ripgrep | tgrep (Rust) | tgrep-go |
|---|---:|---:|---:|---:|
| `handleRequestPayload` | 0 | 2653ms | 73ms | **50ms** |
| `AbortSignal` | 9,146 | 2642ms | 402ms | **163ms** |
| `^import type` | many | 2598ms | 4543ms | **1140ms** |

**1,836 files** (the same repository with its `.gitignore` respected):

| pattern | ripgrep | tgrep (Rust) | tgrep-go |
|---|---:|---:|---:|
| `handleRequest` | 61ms | 26ms | 24ms |
| `error` | 48ms | 47ms | 35ms |
| `^import` | 44ms | 78ms | 44ms |

Index build: Rust 13.3s / Go 52s at 158k files; Rust 0.6s / Go 0.9s at 1,836.
Building is where the Rust is decisively faster.

What the two tables show together is that the index earns its keep on scale and
on selectivity, not unconditionally. At 158k files a rare pattern is **53x**
faster than ripgrep; at 1,836 files the same idea is worth almost nothing, and
on a high-match pattern the reference implementation is *slower* than ripgrep in
both tables — a search returning thousands of matches spends more delivering
them than the index saves finding them, which the upstream README says too.

One caveat on reading any tgrep benchmark: passing a walk-affecting flag
(`--hidden`, `--no-ignore`) to a *search* makes both implementations redo the
walk rather than trust the index, which took the reference from 73ms to 24
seconds on the table above. Build the index with those flags; search without
them.

## License

MIT, matching the upstream project.
