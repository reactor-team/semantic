# Contributing to semantic

Thanks for your interest. This document covers how to build the project, what a
good change looks like, and how to submit one.

## Contents

- Ground rules
- Development setup
- The build tasks
- Making a change
  - Adding a language
  - Changing the chunker
- Tests
  - End-to-end scripts
- Adding a dependency
- Commit sign-off (DCO)
  - Licensing of contributions
- Submitting a pull request
- Releasing
- Reporting bugs
- Reporting security issues

## Ground rules

Be civil. The [Code of Conduct](CODE_OF_CONDUCT.md) applies to every space this
project uses.

Open an issue before you write a large change. A design disagreement is cheap to
resolve in an issue and expensive to resolve in a 900-line pull request.

Small fixes need no issue. Typos, broken links, and obvious bugs go straight to a
pull request.

## Development setup

You need three things:

- **Go 1.26 or later.** The version is pinned in [`.go-version`](.go-version).
- **A C compiler.** The ONNX runtime links via cgo, so `CGO_ENABLED=1` is
  required. On macOS run `xcode-select --install`. On Debian or Ubuntu install
  `build-essential`.
- **[mise](https://mise.jdx.dev).** It pins the Go toolchain, `golangci-lint`,
  and `gotestsum` so your versions match CI.

```bash
git clone https://github.com/reactor-team/semantic
cd semantic
mise trust && mise install
mise run deps
```

If you would rather not install mise, plain Go commands work. Set
`CGO_ENABLED=1` yourself and accept that your linter version may differ from
CI's.

## The build tasks

| Task | What it does |
|---|---|
| `mise run build` | Build into `bin/semantic_<os>-<arch>`, version and SHA stamped. |
| `mise run install` | `go install` into `$(go env GOPATH)/bin` as the global CLI. |
| `mise run test` | `gotestsum ./...` with `CGO_ENABLED=1`. |
| `mise run lint` | `golangci-lint` over the module, `shellcheck` over the task scripts, `actionlint` over the workflows. |
| `mise run fmt` | `gofumpt -w` plus import grouping. |
| `mise run deps` | Download modules and verify `go.mod` and `go.sum` are tidy. |
| `mise run licenses` | Fail if a dependency is copyleft or the license files are damaged. |

Run `mise run fmt && mise run lint && mise run test` before you open a pull
request. CI runs those three plus `licenses`, and will reject a change that
fails any of them.

Run the tasks through mise rather than invoking the scripts directly. Several
are sensitive to the Go version, and mise selects the one pinned in
`.go-version`.

## Making a change

Keep the diff small. A change that renames symbols, reformats untouched code, or
restructures a package as a side effect is hard to review — split the cleanup
into its own commit or its own pull request.

Match the surrounding code. This codebase writes comments that explain *why*, not
*what*, and it writes them at the top of a file or above a non-obvious decision.
Read a neighbouring file before you add your first comment.

Update the docs in the same change. If you alter behavior, flags, or output
format, the README changes with it.

### Adding a language

Source-code indexing is deliberately cheap to extend. Most languages need no Go
code at all:

1. The grammar's Go binding, added to `go.mod`.
2. A `langDef` entry in [`languages.go`](/pkg/chunk/languages.go) naming the
   node kinds that declare a symbol, how each one's name is resolved, and which
   `Variant` it maps to. Nine languages are defined this way — copy the closest
   one.
3. A case in the table in [`registry.go`](/pkg/chunk/registry.go), which maps
   file extensions to the language. That table is the single source both the
   indexer and the `--lang` filter read, so one entry covers both.
4. A case in the table-driven test in
   [`languages_test.go`](/pkg/chunk/languages_test.go).

Nothing else changes. The store schema and the search layer are
content-agnostic.

Write a bespoke walker only when the table cannot express the language without
growing a flag that one caller sets. Seven languages have earned one, and each
names its reason at the top of its file. Read the closest before you decide
yours needs the same.

Two judgment calls dominate the work, and they are worth settling in an issue
first:

- **Which comment carries the doc.** Most languages put it above the signature.
  Python puts the docstring *inside* the body, which is one of the reasons it
  has its own walker rather than a table entry.
- **What counts as a symbol.** Rust has traits and impls; Java has annotations;
  Python has neither interfaces nor enums in the TypeScript sense. Reuse an
  existing `Variant` constant when the concept genuinely matches across
  languages, so search filters stay uniform. Add a new one only when it does
  not.

Use tree-sitter. That is the rule, not a preference: every language goes
through one parser stack, so signature rendering, line numbering, breadcrumb
assembly, and the comment-association logic are written once and shared. A
second parser stack — even a good one, even the standard library's — means
writing all of that again and picking a side for every language after it.

### Changing the chunker

Chunk keys are content addresses, so a chunker change makes existing rows wrong
for files whose bytes never changed. Content-hash diffing cannot see that: every
hash still matches, and a plain `semantic index` would skip the whole vault.

The index guards against this by storing a stamp of what built it, so **bump the
matching version in [`pkg/index/representation.go`](/pkg/index/representation.go)
in the same change**:

| You changed | Bump | Cost of the rebuild |
|---|---|---|
| Any chunker, or the markdown simplifier they share | `chunkVersion` | Re-chunks and re-embeds the whole vault — minutes on a large one. |
| Link extraction — which edges a file yields, or their targets, anchors, kinds, or lines | `linkVersion` | Re-parses edges, touches no vectors — seconds. |

Bump only what actually changed; the two are separate because they cost
different amounts to redo. Then mark the changelog entry **[reindex]**, which is
what tells a user the first run after upgrading will be slow.

Forgetting the bump is the failure this machinery exists to prevent, and nothing
catches it for you: the build passes, the tests pass, and every existing index
silently keeps rows the new code would never produce. Users do not need
`semantic index --force` — a stamp mismatch rebuilds automatically — but that is
only true if you bumped the stamp.

## Tests

Every behavior change needs a test. Bug fixes need a test that fails before the
fix.

Table-driven tests are the house style — see [`pkg/chunk/chunk_test.go`](/pkg/chunk/chunk_test.go). Call
`t.Parallel()` in any test and subtest that can support it.

No test may need the network or a downloaded model in order to pass. The
embedding layer takes an injectable `Embedder`, and `pkg/index` tests pass `nil`
to skip embedding entirely. Follow that pattern.

A test that exercises the real model is allowed on one condition: it skips when
the model is absent. `TestGet_RealModel` in `pkg/embed` and the `[embed]`-gated
end-to-end scripts below both do that. They run for a contributor who has run
`semantic init` and are skipped in CI, so the coverage is free where it exists
and costs nothing where it does not.

### End-to-end scripts

Command behavior — output format, exit codes, which stream a message lands on —
is pinned by [testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript)
scripts in [`cmd/semantic/testdata`](/cmd/semantic/testdata). Each `.txtar` file
builds a vault, runs the real binary against it, and asserts on what came back.
Add one when you change what a command prints or what it exits with.

The scripts stay off the embedding path, because CI has no model. `lint`
indexes a vault without embedding and `--no-reindex` lets the other commands
read what it built — between them most of the program is reachable. A script
that needs the real model declares `[embed]` and is skipped where it is absent,
so CI stays offline.
Open a script with a comment saying which contract it pins and why that contract
matters; the assertions are terse, so the comment is where the reasoning lives.

## Adding a dependency

Prefer the standard library. Prefer a small dependency to a large one. Prefer no
dependency to either.

When you do add one, `mise run licenses` must still pass. It fails the build on
four categories of license:

| Category | Examples | Why it fails |
|---|---|---|
| Forbidden | AGPL, SSPL | Incompatible with Apache-2.0. |
| Restricted | GPL, LGPL | Strong copyleft; the obligation reaches the whole work. |
| Reciprocal | MPL, EPL | File-level copyleft. Survivable, but a maintainer decides it, not a `go get`. |
| Unknown | anything unclassifiable | "We could not tell" is the case the gate exists to catch. |

Do not widen the disallowed list to make the build pass. If a dependency is
genuinely copyleft, find a permissive replacement or open an issue. If it is
permissive but worded oddly enough that the classifier misses it, read the
license yourself, then adjust `CONFIDENCE` in `mise-tasks/licenses.sh` and say
why in the commit message.

`mise run licenses --report` prints the full dependency inventory with each
license.

## Commit sign-off (DCO)

This project uses the [Developer Certificate of Origin](https://developercertificate.org).
It is a one-line assertion that you wrote the patch, or otherwise have the right
to submit it under the project's license. There is no CLA to sign.

Add the sign-off with `git commit -s`, which appends:

```
Signed-off-by: Your Name <your.email@example.com>
```

The name and email must be real and must match your commit author. Pseudonymous
and anonymous contributions are not accepted, because the trailer is the record
of who granted the licence.

Amend a commit you forgot to sign with `git commit --amend -s`, or a whole
branch with `git rebase --signoff main`. Either needs
`git push --force-with-lease` afterwards.

CI checks every commit on a pull request and blocks the merge until each one
carries the trailer.

### Licensing of contributions

No source file carries a copyright or SPDX header. The root
[`LICENSE`](LICENSE) and [`NOTICE`](NOTICE) cover the whole tree, so a new file
needs no comment of its own — this matches the other public repositories in the
org, and it keeps a header out of every diff that adds a file.

`NOTICE` is the exception that has to stay detailed. The tree-sitter runtime
and every grammar are C compiled into the binary, and their licences require
that attribution travel with what ships, so it is listed there rather than
inferred from `go.mod`.

Contributions are accepted under [Apache-2.0](LICENSE), per section 5 of that
license. You keep the copyright on what you write; your `Signed-off-by` trailer
is the only attestation needed.

## Submitting a pull request

- One logical change per pull request.
- Write a description that leads with *why*. The diff already shows what.
- Keep it under 500 lines where you can. Past roughly 1000, split it.
- Link the issue it closes.
- CI must be green.

Maintainers may ask for changes on style grounds. That is not a judgment of the
fix — it keeps the codebase readable by the next person.

## Releasing

Maintainers only. Push a `v*` tag and the release workflow does the rest:

```bash
git tag -a v0.1.0 -m v0.1.0
git push origin v0.1.0
```

Use `-s` instead if you have a signing key configured; every release so far is
annotated and unsigned, and the workflow does not check either way. What a
consumer can actually verify is the build-provenance attestation on each
archive, described at the end of this section.

The tag becomes the version stamped into the binary, so move the `[Unreleased]`
section of [CHANGELOG.md](CHANGELOG.md) under the new heading *before* tagging.
If the release bumps a representation version, say so in that entry: the first
run on the new binary rebuilds part of every existing index, and a user who
reads the changelog should not be surprised by it.

Every archive is built on a native runner for its platform — Linux and macOS,
amd64 and arm64. Nothing is cross-compiled, because onnxruntime_go needs cgo and
the macOS SDK cannot be redistributed. The archives carry the binary only; the
embedding model and the ONNX runtime are still fetched by `semantic init` on
first use. Each archive carries a build-provenance attestation:

```bash
gh attestation verify semantic_v0.1.0_linux_amd64.tar.gz --repo reactor-team/semantic
```

## Reporting bugs

Open an issue using the bug template. The three things that make a report
actionable:

1. The exact command you ran and the output you got.
2. `semantic --version` and `semantic status`.
3. A minimal file that reproduces it, if the bug involves chunking or linting.

## Reporting security issues

Please do **not** open a public issue for a security vulnerability. Report it
through [GitHub Security Advisories](https://github.com/reactor-team/semantic/security/advisories/new)
or email `security@reactor.inc`. We'll coordinate disclosure and a fix, and
credit you in the advisory unless you ask us not to.

Include the version (`semantic --version`), the platform, the steps to
reproduce, and what an attacker gains. A rough report today beats a polished one
next month.

Every changed file is parsed by tree-sitter, which is C compiled into the
binary, so a crafted input that causes a crash, a hang, or memory corruption is
a legitimate finding — send the file. When the cause turns out to be an upstream
grammar rather than our query or our usage, we'll help route it there.

The latest tagged release receives fixes. This project has not reached 1.0, so
there are no long-term support branches.
