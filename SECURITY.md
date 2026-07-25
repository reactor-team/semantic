# Security Policy

## Reporting a vulnerability

Do not open a public issue for a security problem.

Report it privately through [GitHub Security Advisories](https://github.com/reactor-team/semantic/security/advisories/new),
or by email to **team@reactor.inc**.

Include what you have: the version (`semantic --version`), the platform, the
steps to reproduce, and what an attacker gains. A rough report today beats a
polished one next month.

## What to expect

We will acknowledge your report, assess it, and tell you whether we plan to fix
it and roughly when. We will credit you in the advisory unless you ask us not
to.

## Supported versions

The latest tagged release receives security fixes. This project has not reached
1.0, so there are no long-term support branches.

## Threat model

`semantic` is a local, single-user command-line tool. It has no server, no
authentication, and no multi-tenancy — those were deliberately left out of the
design, not overlooked. Understanding what it does touch will tell you whether
something is a vulnerability here.

**It reads the tree you point it at.** `semantic index` walks the vault and reads
every markdown and source file it does not skip. Content lands in a local SQLite
index at `<vault>/.semantic/index.db`. Anyone who can read that file can read
your indexed content — treat the index as exactly as sensitive as the tree it
came from, and keep it out of version control.

**It reaches the network exactly once.** `semantic init` downloads the ONNX
runtime and the embedding model over HTTPS. Nothing else in the tool makes a
network call. Indexing and searching are fully offline. A report that `semantic
search` phoned home would be a serious finding — please send it.

**It parses untrusted input by design.** Chunking runs markdown through goldmark
and source code through tree-sitter. A crafted file that causes
a crash, a hang, or memory corruption in that path is a legitimate
vulnerability. Include the file.

**It links native code.** The ONNX runtime is a shared library loaded at run
time via cgo, and tree-sitter grammars are compiled C. Memory-safety issues in
those paths are in scope for this project when our usage causes them, and belong
upstream when the bug is theirs. We will help route it either way.

### Out of scope

- The `.semantic/index.db` file being readable by other local users. It inherits
  ordinary filesystem permissions, which is the intended design.
- Vulnerabilities in the downloaded ONNX Runtime binary itself. Report those to
  [Microsoft](https://github.com/microsoft/onnxruntime/security). Tell us too, so
  we can bump the pinned version.
- Denial of service caused by pointing the tool at a pathologically large tree.
