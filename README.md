# semantic

Semantic search and hygiene checks over a directory of markdown and source code
— eighteen languages — as a single local Go binary. It embeds your content with a local ONNX model
and cosine-ranks queries against a local SQLite index. **No API key, no network,
fully offline.**

Point it at an Obsidian vault, a `docs/` tree, an engineering-notes folder, or a
codebase, and find things by *meaning* rather than exact keyword — then keep the
corpus healthy: consolidate duplicates, fix dead links, and connect orphaned
notes.

## Contents

- Why
- Languages
  - Filtering by language
- Install
- Quickstart
- What it can do
  - Corpus hygiene, in a bit more detail
- Configuration
- Development
- Contributing
- License

## Why

Grep finds the word you typed. It doesn't find the note you wrote six months ago
that says the same thing in different words, and it can't tell you which two docs
are 90% redundant or which links rot. `semantic` runs a real embedding model on
your machine to answer both kinds of question — retrieval *and* corpus hygiene —
without shipping your notes to anyone.

- **Local & offline.** all-MiniLM-L6-v2 (384-dim) runs via onnxruntime; the
  model + runtime download once (~120MB) and never phone home.
- **Fast & incremental.** Content is chunked, embedded, and stored in SQLite;
  reindexing only re-embeds files whose content hash changed.
- **Safe to upgrade.** The index records which chunker, link extractor, and
  embedding model built it. When a new release changes any of them, it rebuilds
  the affected rows automatically and tells you why — no stale results, and no
  `--force` you had to know to run.
- **Markdown- and code-aware.** Markdown is chunked by its heading tree; source
  by its syntax tree, one chunk per symbol carrying the doc comment and the
  signature — implementation bodies aren't embedded. Eighteen languages, listed
  below.

## Languages

Every language is parsed with [tree-sitter](https://tree-sitter.github.io). The
retrieval surface is a symbol's *documentation and signature*, never its body:
a body is implementation, and embedding it dilutes what the symbol is for.

| Language | Extensions | Chunked as |
|---|---|---|
| Markdown | `.md` `.markdown` | Heading tree |
| Go | `.go` | package · type · func · method · documented const/var |
| Python | `.py` `.pyi` | module · class · method · func · documented constant |
| TypeScript | `.ts` `.mts` `.cts` `.tsx` | func · class · interface · type · enum · const |
| JavaScript | `.js` `.mjs` `.cjs` `.jsx` | func · class · CommonJS export |
| Java | `.java` | class · interface · enum · record · method |
| C# | `.cs` | namespace · class · interface · struct · record · method · property |
| Rust | `.rs` | struct · enum · trait · mod · func · impl method |
| C | `.c` | func · struct · union · enum · typedef |
| C++ | `.cc` `.cpp` `.cxx` `.h` `.hpp` `.hh` | class · struct · namespace · func · method |
| Ruby | `.rb` | class · module · method |
| PHP | `.php` | class · interface · trait · enum · func · method |
| Scala | `.scala` `.sc` | class · object · trait · enum · func |
| Lua | `.lua` | func, including dotted and colon paths |
| Protobuf | `.proto` | message · service · rpc · enum |
| HCL / Terraform | `.tf` `.tfvars` `.hcl` | block, keyed by type and labels |
| YAML | `.yaml` `.yml` | document · top-level key |
| Bash | `.sh` `.bash` | script header · func · documented variable |

Two of those needed a judgment call rather than a translation:

- **YAML** has no declarations, only nesting. Chunking every key floods the
  index; chunking whole files averages a Deployment, a Service, and a ConfigMap
  into one meaningless vector. The unit is the **document** plus its top-level
  keys, and a document that declares `kind` and `metadata.name` is identified by
  them — so it retrieves as `Deployment/api-gateway`, not "the third document".
- **Bash** is mostly top-level commands. Only functions, documented variables,
  and the script's own header comment are indexed; the rest would bury them.

Adding a language is a grammar plus a table entry — see
[CONTRIBUTING.md](CONTRIBUTING.md).

### Filtering by language

`--lang` narrows a search to one or more languages. It is repeatable or
comma-separated, and accepts the names people actually type (`c++`, `k8s`,
`terraform`, `py`). `semantic langs` prints the full list.

```bash
semantic search "session state transitions" --lang go
semantic search "how replicas are set" --lang yaml,hcl
semantic search "retry logic" --lang python --lang go
```

A misspelled language is an error, not an empty result — a search returning
nothing should mean "no such code", never "no such flag value".

## Install

```bash
mise use -g "go:github.com/reactor-team/semantic/cmd/semantic@latest"
semantic init             # one-time: fetch the embedding model + ONNX runtime
```

One prerequisite: **a C compiler.** The ONNX runtime is linked via cgo, so
`CGO_ENABLED=1` is required. The SQLite side is pure Go. macOS:
`xcode-select --install`. Debian: `apt install build-essential`.

To build from a clone instead, use `mise run install`, which puts the binary in
`$(go env GOPATH)/bin` with its version stamped in.


## Quickstart

```bash
cd ~/notes
semantic index                          # build/refresh the index for this tree
semantic search "how retries back off"  # semantic search, ranked by meaning
```

The index lives in `<vault>/.semantic/index.db` — per-directory, gitignorable, and
it travels with the tree.

## What it can do

| Command | What it's for |
|---|---|
| `semantic index` | Incrementally (re)index markdown and source code under the vault (`--vault`, default cwd; honors `.gitignore`). `--force` re-chunks/re-embeds/re-extracts links for every file, even unchanged ones — rarely needed now that an upgrade rebuilds what it invalidates on its own. |
| `semantic search "<query>"` | Rank chunks by cosine similarity to the query; prints `file:line`, breadcrumb, and snippet. |
| `semantic dupes` | Find near-duplicate chunks — redundant docs/guidance worth consolidating. |
| `semantic graph` | Inspect the document link graph: orphans, broken links, broken `#section` anchors, backlinks. |
| `semantic lint` | Flag docs hygiene: inline-code doc/source paths (`` `docs/x.md` ``, `` `pkg/file.go` ``) that should be links (or are ambiguous — a bare basename matching more than one file), deep relative links (`../../`) better written root-absolute, and long files missing an up-to-date `## Contents` TOC. `--fix` rewrites the auto-fixable ones. |
| `semantic status` | Index + model health (DB path, file/chunk counts, last index time). |

Help is the source of truth — every command self-documents:

```bash
semantic --help
semantic <command> --help
```

### Corpus hygiene, in a bit more detail

- **`dupes`** does an all-pairs cosine scan over content-bearing chunks (markdown
  sections *and* source doc-comments) to surface redundancy, cross-file by default.
- **`graph`** resolves `[text](path)` and `[[wikilink]]` edges at query time, so
  renames fix links without rewriting sources. It reports **orphans** (no inbound
  link), **broken** links (target resolves to nothing), and **broken anchors**
  (the file resolves but the `#section` matches no heading) — so linking straight
  to a section is safe and validated. `--backlinks PATH` shows what points at a
  file; `--json`/`--dot` feed other tooling.
- **`lint`** catches three link smells and one structure smell. First, doc or
  source-code references written as inline code (`` `docs/x.md` ``,
  `` `pkg/file.go` ``), which never become graph edges (so a file referenced
  only that way reads as an orphan): split into **unlinked** (resolve to
  exactly one file — worth turning into a link), **ambiguous** (a bare
  basename with no directory, like `` `service.go` ``, matches more than one
  indexed file of that name — promoting it would silently pick whichever
  sorts first, so it's reported with its full candidate list instead), and
  **broken** (dead path or dead `#section`). Second, **deep relative links** —
  real `[text](path)` links that climb two or more directories with `../../`;
  it suggests the equivalent root-absolute path (`/docs/x.md`), which survives <!-- semantic-ignore: illustrative example path -->
  moving the source file. The suggestion is anchored at the enclosing git
  repository root the way GitHub resolves a leading-`/` link, so a vault
  indexed below the repo root (a monorepo or `docs/` sub-tree) gets the right
  prefix (`/subproj/docs/x.md`); with no repo it stays vault-relative. Third, <!-- semantic-ignore: illustrative example path -->
  **missing Contents TOCs** — markdown files over 100 lines whose committed
  `## Contents` table is absent or out of date, so a partial read of a long
  file still reveals its full scope. `--fix` rewrites the auto-fixable
  findings in place: unlinked inline-code refs become real links
  (`` `pkg/file.go` `` → `` [`pkg/file.go`](/pkg/file.go) ``, the original path
  as label, root-absolute so it resolves from wherever it's written), deep
  links to their root-absolute form, and
  Contents TOCs regenerated from the heading tree (a plain-text outline
  directly under the `## Contents` heading); ambiguous and broken refs still
  need a human. False positives suppress ESLint-style with
  `<!-- semantic-ignore -->` on the offending line, `-next-line` on the line
  above, or `-file` at the top to exempt a whole file — the shape a vendored or
  verbatim third-party document wants, since it silences the TOC check too.
  `--unlinked`/`--ambiguous`/`--broken`/`--deep`/
  `--toc` narrow both the report and `--fix` to just the selected kinds
  (`--unlinked --fix` promotes unlinked refs only, touching neither deep links
  nor TOCs). Passing file paths
  (`semantic lint --toc --fix <files…>`) scopes every check and fix to just
  those files — the shape a pre-commit hook wants so it touches only staged
  files, not the whole vault.

## Configuration

| Env var | Meaning |
|---|---|
| `SEMANTIC_DB` | Override the index database path (also `--db`). |
| `SEMANTIC_CACHE_DIR` / `SEMANTIC_MODEL_DIR` | Where the model/runtime are cached. |
| `SEMANTIC_ORT_LIB` | Path to a specific ONNX runtime shared library. |

`search`, `dupes`, `graph`, and `lint` incrementally reindex the vault before
answering (a stat-only no-op when nothing changed), so they never silently
read a stale index. Pass `--no-reindex` to skip this and read the index
exactly as it was after the last explicit `semantic index`.

## Development

```bash
mise run build    # → bin/semantic_<os>-<arch> (version/sha stamped)
mise run test     # gotestsum ./... (CGO_ENABLED=1)
mise run lint     # golangci-lint, shellcheck, actionlint
mise run fmt      # gofmt -w
```

Three more tasks gate a pull request, all of them also run in CI:

```bash
mise run vuln       # govulncheck — CVEs reachable from this module
mise run licenses   # no copyleft dependency, license files intact
mise run deps       # go.mod and go.sum are tidy
```

The command binary is `semantic`; the Go module is
`github.com/reactor-team/semantic`.

## Contributing

Pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers the build,
the house style, and the DCO sign-off; adding a language is the cheapest place
to start and is documented there. Behaviour in every project space is governed
by the [Code of Conduct](CODE_OF_CONDUCT.md).

For a security problem, do not open an issue — see [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE). The embedding model and the ONNX runtime are downloaded
at first use under their own permissive licenses, listed in [NOTICE](NOTICE).
