<!-- semantic-ignore-file: a changelog is a reverse-chronological log, not a document with sections to navigate; a Contents table of version numbers would need regenerating every release and would help nobody -->

# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries that change how content is chunked or keyed are marked **[reindex]**.
Chunk keys are content addresses, so such a change leaves stale rows in an
existing index. The index records which chunker, link extractor, and embedding
model built it and redoes the affected work on the first run after an upgrade,
so this is a note about cost rather than an instruction — `semantic index
--force` is the manual escape hatch.

## [Unreleased]

### Fixed

- **[reindex]** **A TypeScript or JavaScript file's own documentation is now
  indexed.** A leading `/** … */` block is a file's documentation when no
  declaration follows it to own it — the `@fileoverview` and `@module`
  convention, and any design note written above the imports. The walk dropped
  it, because association is by adjacency and an `import`, a bare
  `export {}`, or a re-export carries no symbol to attach it to. Such a file
  therefore contributed only signatures to the index, and the one piece of
  prose saying what the file is for was absent from search. It now emits as a
  `file` chunk keyed `ts/file`, which is for TypeScript what the `package`
  chunk is for Go. A shebang and a directive prologue (`"use client"`) are
  skipped when looking for the block, in either order, and a doc block that
  does document a declaration is untouched.

## [0.1.3] — 2026-08-03

A better default embedding model, a registry so it is no longer the only one,
and progress output for the commands that used to sit silent.

### Changed

- **[reindex]** **The default embedding model is now `arctic-embed-xs`,
  replacing `all-MiniLM-L6-v2`.** Measured against a held-out set of real
  retrieval queries representative of how `semantic` is actually used, it
  ranks results better at roughly two-thirds the download of the other
  candidate default, `bge-small-en-v1.5`. Vectors from the two models are not
  comparable, so the first command that ranks re-embeds the vault and says so
  first. Two details of this checkpoint are load-bearing and both fail
  silently when wrong: it pools at `[CLS]` rather than averaging tokens, and a
  query — never a stored passage — is prefixed with `Represent this sentence
  for searching relevant passages:`. Both are pinned by tests.
- **`graph` and `lint` no longer embed.** Neither command reads a chunk's
  vector, but both used to run the embedder during their automatic reindex,
  which made the first `lint` after a model change re-embed the entire vault to
  produce a result it could not use. They now index with a placeholder vector
  and leave the model stamp alone; the next `search`, `dupes`, or `semantic
  index` fills in exactly those chunks. `lint --no-embed` became a no-op and is
  still accepted, so existing hooks and CI keep working.

### Added

- **`--model` / `$SEMANTIC_MODEL`, and `semantic models` to list them.** The
  registry ships `arctic-embed-xs` (the default), `bge-small-en-v1.5`,
  `bge-small-en-v1.5-int8`, and `all-MiniLM-L6-v2` (kept as the pre-0.1.3
  baseline). Each model caches under its own directory, so switching back
  after the first download costs nothing, and each carries its own dimension,
  sequence length, pooling strategy, and query prefix — the parts that differ
  between checkpoints and quietly produce bad rankings when assumed.
- **`bge-small-en-v1.5-int8`**, the bge checkpoint at a quarter the download
  (33 MB against 127 MB) with no measurable accuracy cost. It is not the
  default because it indexes about 17% slower on Apple Silicon, where ONNX
  Runtime spends more converting around each matmul than int8 arithmetic
  saves. Worth selecting when the download or the disk costs more than
  indexing time does.
- **A guard against ranking across two models.** `--no-reindex` skips the
  rebuild that heals a model change, which previously left a query from one
  model scored against vectors from another: a confident number, and meaningless.
  `search` and `dupes` now compare the index's stamp against the running model
  and refuse with a message naming both.
- **Progress output while downloading and indexing.** A model fetch and a
  full reindex both used to print nothing until they finished, which is
  indistinguishable from a hang. Both now report on a single rewritten line of
  stderr, drawn only to a terminal so redirected output and the end-to-end
  scripts are unaffected.
- **`$SEMANTIC_NO_DOWNLOAD`** makes a missing model an error instead of a
  fetch — for CI, and for anyone who would rather not have a command silently
  pull a checkpoint.

### Fixed

- **A bare `semantic` prints help instead of an error.** With no command it
  exited non-zero with `error: expected one of "init", …` — a newcomer's first,
  most natural invocation met a failure rather than the command list. It now
  prints the same help `--help` does and exits 0.

## [0.1.2] — 2026-07-25

Four corrections to what semantic says, three of them user-facing. Nothing
changes how content is chunked or embedded, so no index is re-embedded; the
link-extractor bump re-parses edges on the first run, which is seconds.

### Fixed

- **`--help` no longer names three languages out of eighteen.** The root
  description and the `index` command both still listed Go, TypeScript, and
  JavaScript — the whole set when those strings were written. Neither
  enumerates now: `semantic langs` is the list, and it is built from the
  registry rather than retyped.
- **A link to `LICENSE` is no longer reported as broken.** An extensionless
  target is treated as a document rather than an asset, so it reaches the
  broken-link check, and nothing indexes a file with no extension — so `graph
  --broken` flagged the `LICENSE` and `NOTICE` links that sit in the README of
  essentially every open-source repository. A target with no extension that
  exists on disk is now resolved by a stat, the same escape the check already
  had for directories.
- **[reindex]** **A `[[wikilink]]` inside inline code is no longer an edge.**
  Markdown links are read off the AST, which knows where inline code is;
  wikilinks are matched over raw source, because goldmark has no node for them,
  and that scan skipped fenced blocks but not inline spans. Prose documenting
  the syntax therefore produced a link — semantic's own README reported a
  broken edge to a note called "wikilink". Only link extraction changes, so the
  first run after upgrading re-parses edges without re-embedding: seconds, not
  minutes.
- **`--force` is no longer described as the way to recover from an upgrade.**
  The flag's help and `CONTRIBUTING.md` both predated automatic rebuilding and
  told users to run a full re-embed that a version stamp already handles.
  `CONTRIBUTING.md` now states the obligation that actually exists, which is a
  contributor bumping `chunkVersion` or `linkVersion` when they change what
  those cover — the step that, if skipped, leaves every existing index holding
  rows the new code would never produce.

### Build and release

- The GitHub Actions used by CI and the release workflow moved to their current
  majors. This is the first release whose archives and provenance attestation
  are produced by them.

## [0.1.1] — 2026-07-25

### Changed

- **No per-file copyright or SPDX headers.** The root `LICENSE` and `NOTICE`
  cover the tree, which is the convention the org's other public repositories
  already follow. Removing them takes a two-line preamble off the top of every
  source file and out of every diff that adds one; `mise run licenses` now
  checks that the two license files are intact instead of policing headers.
  `NOTICE` keeps its detail — the statically linked C grammars require their
  attribution to ship with the binary.
- **The DCO sign-off is enforced.** It was documented but nothing checked it. A
  CI job now fails a pull request carrying a commit without a `Signed-off-by`
  trailer, matching how the other public repositories gate merges.
- **No standalone `SECURITY.md`.** The reporting policy moved into
  `CONTRIBUTING.md`, matching the org's other public repositories, which keep
  one contributor-facing document rather than two. Nothing was dropped: the
  threat model's contributor-facing parts moved with it.

### Fixed

- **The ONNX Runtime and its Go binding now move as a pair.** The binding
  compiles against one release of the C API and loads whatever `semantic init`
  downloaded, so the two versions are not independent. They had drifted: the
  binding expected 1.26.0 and the download fetched 1.24.1, which fails at the
  first embed with `Error setting ORT API base`. Nothing in the test suite
  catches this — inference is skipped when no model is installed — so the two
  are now changed in the same commit or not at all.
- **The runtime is cached per version.** The cache held one unversioned path,
  and the download is skipped when the file is already there, so an upgrade
  would have found the superseded library sitting where the new one belonged
  and never replaced it. Upgrading from 0.1.0 leaves a ~37 MB library behind at
  the old path; it is inert and safe to delete.
- **The Go toolchain resolves to the pinned version.** mise reads `go.mod` in
  preference to `.go-version`, so a `go` directive below the pin selected an
  older toolchain in CI without saying so — surfacing as a license check that
  refused to run and fourteen standard-library vulnerabilities already fixed in
  the pinned release. The two files are now held equal, and `mise run deps`
  fails when they diverge.

Embeddings are unchanged by the runtime upgrade — bit-identical to 0.1.0 — so
no index is rebuilt.

## [0.1.0] — 2026-07-25

Initial public release.

### Added

- **Semantic search over markdown and source code**, offline. Content is
  chunked, embedded with a local ONNX model (all-MiniLM-L6-v2), and stored in
  SQLite; queries are cosine-ranked against it. No API key and no network after
  the one-time `semantic init` fetch.
- **Eighteen languages**, all parsed with tree-sitter: markdown, Go, Python,
  TypeScript, JavaScript, Java, C#, Rust, C, C++, Ruby, PHP, Scala, Lua,
  Protobuf, HCL/Terraform, YAML, and Bash. Markdown chunks by heading tree;
  source chunks one symbol at a time, carrying the doc comment and the
  signature. Bodies are never embedded — a body is implementation, and
  embedding it dilutes what the symbol is for.
  - Two languages needed a granularity decision rather than a translation.
    **YAML** has no declarations, so the unit is the document plus its
    top-level keys, and a document declaring `kind` and `metadata.name` is
    identified by them. **Bash** indexes only functions, documented variables,
    and the script header; a script is mostly top-level commands, and indexing
    those would bury the rest.
  - Nine of them are a grammar plus a table entry
    ([`pkg/chunk/registry.go`](/pkg/chunk/registry.go)) and no Go code at all.
    The rest have a hand-written walker, each because of something a table
    cannot express without growing a flag that one caller sets: Go's package
    clause, Python's docstrings and decorators, Protobuf's rpcs nested in a
    service, HCL's type-and-label naming, and the two granularity decisions
    below.
- **`--lang` on `semantic search`**, with `semantic langs` to list the names it
  takes. Repeatable or comma-separated, with aliases for what people actually
  type (`c++`, `k8s`, `terraform`, `py`). An unknown name is an error rather
  than a filter matching nothing — an empty result should mean "no such code",
  never "no such flag value".
- **Incremental reindexing.** Only files whose content hash changed are
  re-chunked and re-embedded; unchanged files cost a stat. `.gitignore` is
  honoured.
- **Automatic rebuild on upgrade.** The index records which chunker, link
  extractor, and embedding model produced its rows. When a release changes any
  of them the affected work is redone automatically, and the reason is printed
  before the rebuild starts rather than after. A link-extractor change
  re-parses edges without re-embedding, which on a large corpus is seconds
  rather than minutes.
- **Corpus hygiene commands.** `dupes` finds near-duplicate chunks worth
  consolidating; `graph` reports orphans, broken links, broken `#section`
  anchors, and backlinks; `lint` flags inline-code paths that should be links,
  deep relative links, and long files missing an up-to-date `## Contents`
  table. `lint --fix` rewrites the auto-fixable findings.
- **Suppression directives.** `<!-- semantic-ignore -->`,
  `-ignore-next-line`, and `-ignore-file` opt a span, a line, or a whole file
  out of the hygiene checks. A reason may contain `>`, so it can name the
  placeholder it suppresses.

### Build and release

- Apache-2.0, with the root `LICENSE` and `NOTICE` covering the tree. The
  tree-sitter runtime and every grammar are C compiled into the binary, so
  their attribution ships in `NOTICE` with each release rather than being
  fetched at run time.
- `mise run licenses` fails the build if a dependency carries a copyleft or
  unclassifiable license, or if the license files are damaged.
- `mise run lint` covers Go (`golangci-lint`, including `gosec` and `revive`'s
  `exported` rule), shell (`shellcheck`), and workflows (`actionlint`).
- Unit tests plus end-to-end scripts (`cmd/semantic/testdata/*.txtar`) that run
  the real binary against a real vault. Neither reaches the network or needs
  the embedding model: `lint --no-embed` indexes without embedding, and the
  commands that do need a model are asserted to fail by naming `semantic init`.
- `mise run vuln` runs `govulncheck`, failing only on vulnerabilities with a
  call path reachable from this module.
- GitHub Actions CI covering test, lint, build, tidy, vulnerabilities, and
  license compliance.
- Tagged releases. Pushing a `v*` tag builds an archive for Linux and macOS on
  amd64 and arm64, each on a native runner — onnxruntime and the grammars need
  cgo, so nothing is cross-compiled — and publishes them with checksums and a
  build-provenance attestation. The archives carry the binary only; `semantic
  init` fetches the model and runtime on first use.

[Unreleased]: https://github.com/reactor-team/semantic/compare/v0.1.3...HEAD
[0.1.3]: https://github.com/reactor-team/semantic/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/reactor-team/semantic/releases/tag/v0.1.2
[0.1.1]: https://github.com/reactor-team/semantic/releases/tag/v0.1.1
[0.1.0]: https://github.com/reactor-team/semantic/releases/tag/v0.1.0
