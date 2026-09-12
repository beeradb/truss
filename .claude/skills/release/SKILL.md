---
name: release
description: Cut a release, or work out why a deployment is still running the old one. Use when tagging a version, when changing release.yml or tofu-versions, and when a merged fix has not reached the thing it fixes.
---

# Releasing

## A merge is not a delivery

Truss is run by somebody else's cluster, pinned by digest. Until a release is
cut *and* that digest is repinned in the consumer's reviewed diff, a merged fix
has changed nothing. v0.1.8 shipped a guard that refused every play in a live
deployment; merging the fix left it refusing, because the pin still named
v0.1.8. When asked why a fix has not taken effect, ask in this order: is it
tagged, is the image built, is the digest pinned, has the applier restarted.

## The tag is tested again, not trusted because main was green

A tag can point at any commit, including one that never sat on `main` and never
passed `ci.yml`. `release.yml` therefore re-runs vet, `go test -count=1`,
`leakscan-test` then `leakscan`, and `check-observability` — a release is the
artefact people run. Do not thin that out because CI already passed.

⚠️ **`release.yml` restates `ci.yml`'s dependency setup instead of running
`scripts/check`, and that has already cost a release.** `tofu` is a real test
dependency — `internal/plan`'s declaration tests fail rather than skip without
it — and it was installed in `ci.yml` only. Every release after that change was
broken and nothing noticed, because nothing was tagged in between; v0.1.4 found
it. The duplication is recorded in `docs/work-items.md`. Until it is fixed, a
new test dependency goes into **both** workflows in the same commit.

## One image per OpenTofu version, and the version is in the tag

A consumer's CI plans with one tofu and its applier re-plans with the one in
this image. If they differ, every plan digest mismatches and every apply is
refused — so the pin is explicit in the tag rather than a coincidence.
`tofu-versions` is the list. Adding a line ships another image; removing one
strands whoever pinned it, so removing a version is a consumer-visible change
and belongs in release notes.

## What the run does that looks like a failure and is not

- **The release may already exist.** Creating a release in the web UI is what
  creates the tag, so the ordinary human path makes `gh release create` fail
  with "already exists" — *after* the images are pushed. The workflow checks
  `gh release view` first for exactly that reason; assets upload with
  `--clobber` so a re-run is idempotent.
- **Attestation fails on a private repository** unless the account is on
  Enterprise Cloud. This repository is intended to be public and is not yet, so
  the first release cut while it is private goes red there. Know it before the
  release rather than during it.
- **The digest in `IMAGE_DIGESTS.txt` is bare hex before the first space**, not
  `sha256:…`; the attest action splits on that space and validates hex. The
  human-readable `IMAGE_DIGESTS` is the other file.

## Reproducibility is a check, not a claim

Binaries build with `-trimpath` and no build stamp, then build again and `cmp`.
A binary carrying a path or a timestamp differs per machine, which makes a
published sha256 an assertion nobody can verify. If that step goes red,
something started embedding state — fix that, do not drop the check.

## Handing it over

Give the consumer the **digest**, from the release assets. A tag makes "what ran
last night" unanswerable, and repinning a tag changes what runs with no diff
anybody reviewed.
