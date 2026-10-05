# Linux release smoke test — Implementation Plan

**Goal:** A CI job that installs a just-published release exactly as a real
user would (`curl | sh`, downloading from GitHub Releases) and exercises the
systemd install/upgrade/uninstall path for real — closing 5 of #957's 6
checklist bullets.

**Architecture:** Triggers on `workflow_run` of the workflow named
`"Release binaries"` finishing (`types: [completed]`, `conclusion ==
'success'`) — matched by that exact name, not by filename, which is why the
name has to be byte-identical to `release-binaries.yaml`'s own `name:` field.
Triggering on this rather than the tag/main push directly guarantees the
release assets already exist rather than racing the upload. Maps the
triggering run's `head_branch` to the tag under test (`v*` as-is; `main` →
`main-latest`, the same `CHANNEL_TAG` `scripts/install.sh` itself uses).
Reuses the exact `enable-linger` + wait-for-`/run/user/<uid>/bus` recipe
`ci.yaml`'s `agentop` leg already uses for the real-systemd integration test
(#1076) — proven to work on GitHub-hosted `ubuntu-latest` runners.

The actual test logic lives in `scripts/release_smoke_test_linux.sh`, kept
out of the YAML for the same reason `deploy/proxy-init/test-enforce-redirect.sh`
is a separate script rather than inline `run:` steps. It downloads
`install.sh` to a temp file and feeds that in on stdin, rather than invoking
the checked-out copy as a local file argument — `install.sh`'s own re-exec
bootstrap only re-fetches the tagged copy of itself (matching the requested
`--ref`) when `$0` isn't a readable file, which is the condition a real
`curl | sh` user hits (and a local file invocation would skip) just as much
as a literal shell pipe does.

**Spec:** none — #957 is its own checklist, not a design doc.

**Issue:** cortex #957

## Tasks

- [x] `scripts/release_smoke_test_linux.sh`: fresh install of the most recent
      stable release before the tag under test (auto-detected via
      `gh release list`, not hardcoded — a fixed "known good" version would
      drift out of the release list over time), assert `agentop service status`
      reports healthy.
- [x] Upgrade to the tag under test: a config marker only this test writes,
      asserted still present after the upgrade (proves `migrateConfig`'s
      additive-only behavior holds beyond just its own listener pins),
      re-assert healthy.
- [x] No-op re-run of the same tag: assert `agentop service install` reports
      `"Already current"` rather than re-installing.
- [x] `agentop service uninstall`: assert `"Removed"`, the unit file is gone,
      and `~/.cortex/config.yaml` is untouched (per uninstall's own promise).
- [x] `.github/workflows/release-smoke-linux.yaml`: the `workflow_run`
      trigger, tag resolution, systemd session setup, and the script
      invocation.
- [x] `shellcheck --severity=error` and `yamllint` (repo's relaxed config)
      both clean locally.
- [x] Fix (review): `workflows: ["Release Binaries"]` didn't match
      `release-binaries.yaml`'s actual `name: Release binaries` (lowercase
      `b`) — a `workflow_run` name mismatch fails silently, no run is ever
      created. This meant the trigger never fired at all until fixed.
- [x] Fix (review): `github.event.workflow_run.head_branch` and
      `steps.tag.outputs.tag` were interpolated directly into `run:` shell
      bodies rather than passed through `env:` first — a standard GitHub
      Actions hardening issue (flagged independently by `zizmor`) regardless
      of how constrained the practical input space is here.
- [x] Fix (review): the older-release lookup used `|| true`, which cannot
      distinguish "no earlier release exists" from "`gh` failed" — and used
      "the most recent *other* release" rather than "the most recent release
      *before* the tag under test," which breaks if this workflow is
      triggered by re-running an old completed Release Binaries run (a real
      possibility) once newer releases already exist. Fixed by anchoring on
      `createdAt` (not `publishedAt`, which stays frozen at first-creation
      for a rolling tag like `main-latest` whose assets get clobbered on
      every push to main — verified against the live API) and checking each
      `gh` call's own exit status explicitly, since dash has no `pipefail`.
- [x] Fix (review): `curl | sh` piped straight through would, on a failed
      download, leave `sh` reading empty stdin and exit 0 under dash (no
      pipefail) — the failure would only surface later at `assert_healthy`,
      pointing at the wrong thing. Now downloads to a temp file first and
      checks it explicitly, still reading from a file via stdin redirection
      (not as a named file argument) so `$0` stays non-file and install.sh's
      re-exec bootstrap still triggers.
- [x] Fix (review, self-caught while implementing the above): piping
      `install_cortex | tee` would have silently swallowed the function's own
      new `exit 1` calls — each side of a pipe runs in its own subshell under
      dash, so an `exit` inside the left side only kills that subshell, and
      the pipeline's reported exit status comes from `tee` (0) regardless.
      Fixed by redirecting to a file and checking the function's own exit
      status directly instead of piping through `tee`.
- [x] Fix (review): uninstall's config check asserted only that the file
      still exists, not that its contents are unchanged — existence survives
      truncation or a rewrite. Now compares a `cksum` taken immediately
      before uninstall against one taken after.
- [x] Fix (review): each `mktemp` call had its own scattered `rm -f` after
      use, which an early exit would skip. Matched the existing pattern in
      `scripts/install_test.sh` and `scripts/dev/verify-moved-ca-diagnostics.sh`
      instead: one `TMP_DIR`, one `trap 'rm -rf "${TMP_DIR}"' EXIT` set once,
      cleaning up on every exit path rather than trusting each call site to
      remember its own.
- [x] Fix (review): rebased onto main and swept `abctl`→`agentop`,
      `authbridge-proxy`→`cortex` — #1203 shipped that rename while this
      branch was in flight (branch was 133 commits behind), and it already
      shipped in `v0.8.0`/`v0.8.1`. `cortex.service` (the unit name) and
      `~/.cortex/config.yaml` (the proxy's own config) are unaffected by
      either rename, confirmed directly against current `cmd/agentop/`.
- [x] Fix (review): the fresh-install leg can resolve `INSTALL_TAG` to a
      pre-rename release (confirmed still live: testing `v0.8.0` itself
      resolves its own "older" release to the pre-rename `v0.7.0`), whose
      own `install.sh` installs `abctl`, not `agentop`. Added `detect_cli`
      for that one call site only — every step after the upgrade installs
      `TAG` itself, which is always current by construction.
- [x] Fix (review): `install_cortex` called `exit 1` on a failed download,
      but it's invoked under `if !` at the no-op-re-run call site — `exit`
      inside a function terminates the whole script even there, skipping
      that call site's own diagnostic `cat` entirely and leaving a bare red
      CI run with the failure message trapped in a file nobody prints.
      Changed to `return 1`; verified under `dash` that the caller now sees
      both the failure and the captured output.
- [x] Fix (review): the upgrade leg asserted health and the config marker,
      but never that the *new* binary was actually serving — exactly the
      regression #1203's own systemd fix addressed (pre-fix, a stale old
      process could keep serving after "upgrade" while status reported
      healthy and a re-run said "Already current"; this leg's existing
      checks all pass in that broken state). Added
      `assert_running_binary_is_current` (compares `/proc/<pid>/exe` against
      the installed binary, not the unit's active/inactive state) and
      `assert_running_version_is` (checks `cortex --version` against the
      tag, or `main-<7-char sha>` for `main-latest` — ties a `main-latest`
      run to the exact commit that triggered it, since a later merge can
      re-point the tag and clobber its assets while this job is still
      running).
- [x] Fix (review): the `createdAt` explanation was right in conclusion but
      wrong about the mechanism — it isn't that "assets get clobbered,
      createdAt refreshed" as a general Releases API behavior. What actually
      happens: `release-binaries.yaml` PATCHes `main-latest`'s own tag ref to
      the triggering commit on every push to main, and `createdAt` follows
      the commit its tag points at. Corrected the comment accordingly.
- [x] Fix (follow-up, decided in conversation): dropped `detect_cli` entirely
      — matches this repo's own clean-break policy for the abctl rename (no
      alias, no compatibility code) more closely than keeping a one-off
      fallback for a single already-superseded release. Re-testing `v0.8.0`
      itself now fails plainly rather than falling back to the pre-rename
      CLI; accepted, since that case only affects one historical release and
      the test's "no older release" first-run path isn't affected.
- [x] Fix (review, round 2 — a separate PR landed `agentop setup`
      mid-review): `install.sh` was rearchitected (#1203's follow-up) to hand
      installation off to a new `agentop setup` subcommand instead of doing
      it directly. Verified before touching anything further: the on-disk
      layout, `agentop service uninstall`, the SHA-256 "already current"
      stamp check, and the `enable`+`restart` split from #1203's own fix are
      all unchanged in substance, just invoked one layer up through
      `setup`'s step pipeline — none of this script's assertions needed to
      change because of it, confirmed by reading the new
      `cmd/agentop/cmd_setup.go`/`setup_step_*.go` directly rather than
      assuming.
- [x] Fix (review, round 2): the workflow's checkout pinned
      `ref: ${{ github.event.workflow_run.head_sha }}` — the release's own
      commit. This script didn't exist yet at `v0.8.0`/`v0.8.1` (confirmed
      via the GitHub API: 404 for `scripts/release_smoke_test_linux.sh` at
      both commits), so re-running an old release's workflow failed before
      the test even started. Dropped the `ref:` pin — the checkout is only
      for the test script itself; the release assets and `install.sh` both
      come over the network regardless of what's checked out.
- [x] Fix (review, round 2): `--limit 20` on the release lookup capped the
      candidate pool BEFORE filtering, and date-based "older than TAG" broke
      across maintenance branches (`release-0.6`/`release-0.7` both exist; a
      patch tagged later on an older line could resolve to something with a
      *higher* version number). Replaced the whole lookup: `main-latest`
      just takes the single newest stable release (`--limit 1`, no date
      comparison needed); a real `v*` tag now compares by VERSION NUMBER
      (`sort -V` across up to 200 stable releases, not a 20-release
      date-ordered cap) rather than by creation date. Verified against the
      live release list and a simulated maintenance-branch case
      (`v0.7.1` tagged after `v0.8.1`) that it resolves to `v0.7.0`, not
      `v0.8.1`.
- [x] Fix (review, round 2): `assert_healthy` still piped
      `agentop service status | tee`, losing the command's own exit status
      under dash — a missing `agentop` (exit 127) would read as "did not
      report healthy" rather than "command not found". Checked directly now,
      same pattern already used for `install_cortex`.
- [x] Fix (review, round 2): uninstall's three checks (printed "Removed",
      unit file gone, config unchanged) can all pass even if the proxy never
      actually stopped — `removeService` only logs a failed
      `systemctl --user disable --now`, then deletes the unit file and
      prints success regardless. Added a direct
      `systemctl --user is-active --quiet cortex.service` check.
- [x] Fix (review, round 2): a `main-latest` run pinned the expected
      *version string* to the triggering commit, but not the *installer
      script* used to install it — that was always fetched from
      `raw.githubusercontent.com/.../main/...`, cached up to 5 minutes. A
      commit that changes the installer and the binaries together (as the
      rename PR did) has a narrow window to be tested with a mismatched
      pairing. `install_cortex` now fetches the installer from a SHA-pinned
      URL whenever installing `main-latest`.
- [x] Fix (review, round 2): a couple of this file's own comments overclaimed
      — one said a cost "stops being reachable... once a newer stable
      release exists" when `OLDER_TAG` is actually anchored to "before TAG"
      permanently; another said a pre-rename re-test fails with
      "command not found" when it actually surfaced as a misleading
      "did not report healthy" (now fixed to fail as a direct, checked exit
      status instead). Corrected both. Also fixed capitalization drift
      ("Release Binaries" vs the workflow's real name, "Release binaries")
      in prose comments that didn't get the trigger's own earlier fix.

## Result

5 of #957's 6 bullets closed: fresh install from a published tag, healthy
status, upgrade-with-config-preserved, idempotent re-run, and clean
uninstall — all against a real release, on a real Linux runner, via the same
`curl | sh` path a real user takes. Runs on every `v*` tag and on
`main-latest`, automatically, once `release-binaries.yaml` finishes
publishing either.

Not closed: the third bullet (drive a real request through the proxy and
assert a parsed event with a non-zero token count) needs the unattended/
headless capture path from #955, which doesn't exist yet — tracked there.
arm64 coverage is explicitly out of scope: no real arm64 GitHub-hosted
runner exists in this org, and no arm64-*execution* pattern exists anywhere
in this repo's CI today (only cross-compilation/QEMU image builds, never a
running arm64 binary) — extending that is new infrastructure, not a small
addition to this workflow.
