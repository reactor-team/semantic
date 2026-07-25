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

[Unreleased]: https://github.com/reactor-team/semantic/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/reactor-team/semantic/releases/tag/v0.1.1
[0.1.0]: https://github.com/reactor-team/semantic/releases/tag/v0.1.0
