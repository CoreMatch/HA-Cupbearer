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

## Rule 4 — `git rm --cached` can still cost you the working-tree file

Untracking a file is **not** guaranteed to be index-only. When the untracked
file has a same-content sibling (e.g. you copied `config.yaml` to
`config.yaml.example` first), the later `git merge` applies **rename
detection** and physically moves/deletes the on-disk file while reconciling
the working tree — the file you meant to keep is gone after the merge.
Symlinks are deleted the same way.

Procedure whenever you untrack something that must stay on disk:

```sh
git rm --cached <path>        # stage the untrack
# ... commit, merge, push ...
ls -la <path>                 # IMMEDIATELY verify the file is still on disk
```

If it is missing, restore it at once from the tracked template
(`cp <path>.example <path>`) — a running instance may depend on it. Do not
consider an untrack done until the on-disk file has been re-verified.

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

## Incident record — 2026-11-08: untrack-by-`rm --cached` still moved working-tree files

Second "thought it was index-only, actually touched the working tree" case.
While converting tracked configs to `.example` mode in sibling repos
(HRPAuth-Yggdrasil-API `config.yaml`, MatchCoreArena-Server
`config/tokens.yaml`) and untracking absolute-path symlinks (HASkinLib,
MatchCoreArena-MaintenanceView), each `git rm --cached` was staged and
committed correctly — but the subsequent `--no-ff` merge applied rename
detection (file vs its just-added `.example` copy) and **physically
moved/deleted the on-disk files** the instances depend on.

- Caught and recovered the same day: files restored from their `.example`
  twins (byte-identical content), running instances unaffected; symlinks
  recreated with their original targets.
- Root cause: merge-time rename detection reconciles the working tree, not
  just the index (Rule 4).
- Countermeasure: Rule 4 — verify the on-disk file immediately after any
  untrack-and-merge, restore from `.example` at once if missing.
