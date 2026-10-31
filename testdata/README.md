# testdata

`fixture/` is a small deterministic corpus of plain files.

It is deliberately NOT a git repository. Gitignore rules only apply inside one,
so a fixture that needed them would have to be — but this one carries no
`.gitignore`, so the marker cannot change which files are admitted (verified:
`tgrep --files` returns the same five paths with and without it). Leaving it out
keeps this directory from being a nested repository inside the project's own,
which git cannot track without a submodule.

Fixtures that DO exercise ignore rules are built at test time by `tree()` in
`compat/walker_test.go`, which runs `git init` in a temp directory.

`refindex/` is the index the **reference Rust implementation** wrote for that
fixture. The tests in `compat/` read it and assert this port agrees with it,
byte for byte where the format is deterministic.

Regenerate both after changing the fixture:

```sh
# from the tgrep-go checkout, with the Rust tgrep built at ../tgrep
../tgrep/target/release/tgrep index testdata/fixture --index-path testdata/refindex
```

`refindex/meta.json` records an absolute `root_path` from the machine that
generated it. Nothing compares that value — the round-trip test loads it and
writes it back, so any path works — and it is left as the reference wrote it
rather than doctored, since the point of this directory is to hold the
reference's real output.

It was regenerated with reference v1.2.6, which writes format version 3: a
versioned `files.bin` header, and `meta.json` bound to it by a blake3
`file_table_id`.

Three of the six files are **not** byte-comparable and the tests only check them
semantically:

- `filestamps.json` serialises a hash map, whose iteration order the Rust
  implementation randomises per process.
- `meta.json` carries `created_at`/`updated_at` and an absolute `root_path`,
  so it only round-trips against itself, not across machines.
- `files-extra.bin` is the v2 sidecar, carrying hidden-file visibility this
  port does not record; its paths are compared instead.

The other three — `lookup.bin`, `index.bin`, `files.bin` — are fully
deterministic and are compared byte for byte.
