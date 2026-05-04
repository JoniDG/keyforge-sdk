<!--
Title format (Conventional Commits):
  feat: short description
  fix: short description
  chore: short description
  refactor: short description
  docs: short description
  test: short description
-->

## Summary

<!-- 1-3 sentences describing what this PR does and why. -->

## Affected language(s)

- [ ] Go SDK (`go/`)
- [ ] Node SDK (`node/`)
- [ ] Python SDK (`python/`)
- [ ] Protocol docs (`docs/`)
- [ ] Examples (`examples/`)

## Changes

<!-- Bullet list of the technical changes. -->
- 

## API impact

- [ ] Backwards-compatible
- [ ] BREAKING (requires major version bump)

## Test plan

- [ ] `make test` passes (in the affected language directory)
- [ ] `make lint` is clean
- [ ] Coverage ≥ 95% (`make cover`)
- [ ] Same semantics across languages (when adding the same feature in multiple SDKs)

## Checklist

- [ ] Branch named `feature/...`, `fix/...`, `chore/...`, etc.
- [ ] Commits follow Conventional Commits (`feat:`, `fix:`, `chore:`...)
- [ ] CLAUDE.md updated if conventions changed
- [ ] Public API documented (godoc / TSDoc / docstring)
- [ ] No identity leakage (defensive grep clean)

## Notes for reviewer (optional)

<!-- Anything tricky, context, or follow-ups. -->
