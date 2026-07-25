<!--
Lead with why. The diff already shows what changed.
Small fixes need no ceremony — delete the sections that do not apply.
-->

## Why

<!-- What problem does this solve? Link the issue it closes. -->

## What changed

<!-- The shape of the change, at the level of a reviewer orienting themselves. -->

## Checklist

- [ ] `mise run fmt && mise run lint && mise run test` pass locally
- [ ] `mise run licenses` passes (required if this touches `go.mod`)
- [ ] Tests cover the change; a bug fix has a test that failed before it
- [ ] Commits are signed off (`git commit -s`) — see [DCO](https://github.com/reactor-team/semantic/blob/main/CONTRIBUTING.md#commit-sign-off-dco)
- [ ] README updated if behavior, flags, or output changed

## Index compatibility

<!--
Delete this section unless it applies.

Does this change how chunks are keyed, chunked, or how links are extracted?
If so, existing indexes keep stale rows and users need `semantic index --force`
to recover. Say so here so it reaches the changelog.
-->
