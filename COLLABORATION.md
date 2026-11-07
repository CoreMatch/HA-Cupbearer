# Collaboration Rules (HA-Services-Deployer)

This repository is frequently worked on by multiple sessions/developers in the
same working tree (orchestration feature work, template maintenance, docs).
The rules below exist because violating them once already caused a premature
push of someone else's work-in-progress.

## Rule 1 — Inspect the index before every merge and commit

`git merge` commits **the index**, not only your branch. Before any
`git merge` or `git commit`, run:

```sh
git diff --cached --stat   # what is staged right now
git status --short         # staged vs unstaged vs untracked
```

If anything you did not stage appears in `git diff --cached`, a parallel
session staged it. Unstage it (`git restore --staged <path>`) or coordinate
before merging — otherwise the merge commit will carry their work-in-progress
into main and push it prematurely.

## Rule 2 — Stage by explicit path in shared trees

Never `git add -A` / `git add .` in this repository while parallel work may be
in progress. Always `git add <explicit paths>` for your own change set.

## Rule 3 — Check the tree before branching

`git status --short --branch` first. If the tree carries someone else's
uncommitted changes, either wait, or isolate your work (separate worktree, or
strictly path-scoped staging) and never commit their files.

## Incident record — 2026-11-08: merge swept a parallel session's staged WIP

Merge `d261c0f` ("Merge branch 'docs/template-reconciliation'") unintentionally
committed a parallel session's **staged** work-in-progress together with the
docs merge: `internal/orchestrator/smoke.go` + `smoke_test.go`,
`internal/manifest/{manifest,validate,validate_test}.go` extensions,
`internal/orchestrator/preflight*.go` additions, `internal/config/credentials.go`,
`knowledge/0007` updates, README and `deploy-manifest.yaml` changes.

- Verified immediately after: `go build && go vet && go test ./...` green on
  main; no work was lost or overwritten (the WIP lives in `d261c0f` and
  remained in the working tree).
- Author decision: **no history rewrite** — the risk of rewriting a pushed
  main outweighs the cosmetic cost of an early landing.
- **Notice to the owning session**: your orchestration smoke-test/preflight
  work is already on `main` as of `d261c0f`. Continue on top of main; do not
  re-commit or re-apply those files; if another worktree still carries them
  staged, run `git restore --staged .` there and rebase/diff against main
  before continuing.
- Root cause: the parallel session had staged its files; the docs merge then
  committed the index (Rule 1).
