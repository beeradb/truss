# The toolchain

Everything `scripts/check` needs, at the versions this repository is pinned
to, installed by one command and verified against each project's own published
checksum.

    scripts/toolchain check              what is installed, what is missing
    scripts/toolchain install            everything
    scripts/toolchain install go jq      just those
    scripts/toolchain manifest           what it would install, and from where

Nothing needs root. Binaries land in `$TRUSS_TOOLCHAIN_PREFIX/bin` (default
`~/.local/bin`) and the Go tree in `.../lib/go`, so the same command works on
a developer's box, inside a container build, and from whatever provisions a
machine.

**Linux, amd64 or arm64** — the two this repository ships binaries for.
Anything else is refused in the first second, by name, rather than after
downloading and unpacking an archive and then failing on a version
comparison.

## What it installs, and why each one is there

| Tool | Version pinned in | Needed for |
| --- | --- | --- |
| Go | `go.mod` | building and testing everything |
| jq | `scripts/toolchain` | the plan digest's differential test |
| OpenTofu | `tofu-versions` | verifying plan JSON shapes by measurement |

⚠️ **The patch release is part of the pin, and it matters more than it
looks.** `go.mod` says `go 1.25`, which `actions/setup-go` resolves to the
newest 1.25.x — so a table here pinning 1.25.0 installed a compiler fourteen
patch releases behind CI's, and govulncheck found 27 reachable
standard-library vulnerabilities in it. Track the patch release CI resolves
to, and let govulncheck say when it has moved.

**Go** is pinned by `go.mod` and nowhere else. `scripts/toolchain` refuses to
run if its own table disagrees with it, because a toolchain that differs from
CI's is how "it passes locally" stops meaning anything.

⚠️ **That guarantees the minor, not the patch.** `go.mod` says `go 1.25`, and
`actions/setup-go` reading `go-version-file` resolves that to the newest
1.25.x — so CI's patch version moves on its own and this table's cannot
follow it. What the check prevents is somebody bumping one *minor* without
the other. If a patch-level difference ever matters, the fix is to pin CI
explicitly, not to loosen this.

**jq** is a test dependency and its absence is not a skip you can ignore.
`internal/plan` must produce bytes identical to what `jq -S -c | sha256sum`
writes — the one byte-identical requirement in the system — and the only thing
that proves it is a differential test running the real pipeline. Without jq
that test skips. `TRUSS_REQUIRE_JQ=1` turns the skip into a failure; CI sets
it, and so should anything else that claims to have run the checks.

⚠️ **The version is chosen to match CI's, not to be the newest.** CI runs on
`ubuntu-latest`, whose jq is 1.7.1, and `internal/plan/digest.go` records its
escaping behaviour as measured against jq 1.7. Installing a different minor
version here would make the differential test compare against something CI
never runs, and a divergence it found would be a fact about your laptop.

⚠️ **`tofu-versions` is a list, not a single version.** `release.yml` reads it
line by line — one image per supported OpenTofu version — so what the
installer requires is that the version it installs is *one of* the lines,
not that the file holds exactly one.

**OpenTofu** is not needed by `scripts/check` and is installed anyway, because
several open questions in [work-items.md](work-items.md) are blocked on
nobody having a `tofu` binary to measure a real `tofu show -json` with. The
version matches `tofu-versions`, which is what the applier runs.

## What it does not install

`gcloud`, because nothing in the checks calls it:
`scripts/ledger-retention-test` drives the real script with a fake `gcloud` on
`PATH`. You need the real one only to run `ledger-retention` against a real
bucket, which is an operator action and not a test.

## How the verification works

`scripts/toolchain.sha256` holds the checksums in `sha256sum -c` format, taken
from each project's own published checksum file rather than computed from a
download — a checksum computed from the file you just fetched proves only that
it arrived intact, which is not the question.

A download that dies without warning — a SIGKILL, a lost machine — leaves a
partial file behind, so `install` removes any whose owning process is no
longer running before it fetches anything. One being written by a concurrent
install is left alone, because the file is named for the process writing it,
and existence is read from `/proc` rather than by signalling: a process
belonging to another user cannot be signalled and is emphatically not dead.
Only files this user owns are considered at all — `/proc` cannot be trusted
about another user's processes on a host with `hidepid` set, and a sweep that
cannot finish is not allowed to stop an install.

⚠️ **A mismatch is fatal, installs nothing, and deletes the archive** — or
says it could not. There is no second URL and no retry: an archive that is not
the one this repository pinned is a supply-chain failure, not a transient one.
Deleting it matters because a refused archive left in the cache is read back
on the next run and refused again, with no clue that the fix is to remove it;
on a cache this user cannot write, the refusal says so rather than claiming a
deletion that did not happen.

`scripts/toolchain-test` proves all of that still refuses, and runs in
`scripts/check`. Every guard in it has been watched failing.

## Building infrastructure around it

The script is the interface; the tables inside it are not. Three things are
stable enough to depend on:

- **`scripts/toolchain install` is idempotent.** A tool already at the pinned
  version is left alone and reported, so it is safe to run on every
  provision, in a `RUN` layer, or from a configuration-management pass.
  ⚠️ Idempotent is not concurrent: two installs of the same tool into the same
  prefix are one job run twice, and nothing here arbitrates between them.
  Different tools no longer collide (each stages in its own directory, and
  each download writes a part file of its own), which is enough for a
  provisioning pass that fans out by tool.
- **`scripts/toolchain check` exits non-zero** when anything is missing, at
  the wrong version, not on `PATH` at all, or *shadowed* — an older `go`
  earlier on `PATH` is a failure even though the prefix holds the right one,
  because the question the subcommand answers is which binary the checks will
  actually run. That is the
  assertion to run after provisioning, and the one to run before trusting a
  green check on an unfamiliar machine.
- **`scripts/toolchain manifest`** prints what this host would install, as
  `tool|arch|version|filename|url` records. Seed a cache offline from the
  filenames, or report what a provision put on a machine from the versions,
  without restating a pin this script owns.
- **Two environment variables** are the whole configuration:
  `TRUSS_TOOLCHAIN_PREFIX` (where it installs) and `TRUSS_TOOLCHAIN_CACHE`
  (where archives are kept). Set the cache to a persistent path and a rebuild
  re-verifies rather than re-downloads; seed it by hand and the install works
  with no network at all.

Budget about **500 MB** in the prefix and **100 MB** in the cache. Measured
2026-09-09 on linux/amd64: the unpacked Go tree is 242 MB, the tofu binary
110 MB, jq 2 MB; the archives are 60 MB (Go), 35 MB (tofu) and 2 MB (jq, a
bare binary rather than an archive).

⚠️ **The prefix figure is steady state.** A Go upgrade unpacks the new tree
beside the old one before replacing it — that is what makes a bad archive
refusable without destroying a working install — so an in-place bump wants
roughly 600 MB for the duration, and about 700 MB if the cache is left at its
default underneath the prefix.

⚠️ **And the first measurement of the cache was wrong**, because it was taken
after a run that never fetched tofu: it was already at the pinned version, so
its 35 MB was missing from what the cache held. A number measured from a run
that did not do the thing is not a measurement of the thing.

⚠️ **Do not point the cache or the prefix at `/tmp` without looking at it
first.** On the machine this was written on `/tmp` is a 1.9 GB tmpfs shared
with everything else running; caching the archives there and unpacking Go
there as well exhausted it — an extracted Go tree beside its archive and
whatever else was already using that tmpfs — and every process needing a
temporary file, the shell included, started failing with `EDQUOT`. The script now stages
alongside its destination for that reason, which also turns a cross-filesystem
copy of the whole Go tree into a rename.

## What CI does instead, and why that is not a contradiction

`.github/workflows/ci.yml` installs Go with `actions/setup-go` reading
`go-version-file: go.mod`, and uses the runner image's jq after asserting it
exists with `jq --version`. For those two it does not run `scripts/toolchain`,
because the runner already has a package manager, a cache and a pinned image,
and adding a second installer would be a second statement of the same fact.

⚠️ **It DOES run `scripts/toolchain install tofu`, and the difference is where
the tool comes from.** Go arrives from an action and jq from the runner image;
OpenTofu arrives from neither, so there is no second statement to avoid — there
is only this script or a bare download beside it. `internal/plan`'s declaration
tests generate real plan JSON and read it back, because the shape they rely on
(a provisioner nested inside a module) is a fact about OpenTofu's output rather
than about this code, and a hand-written fixture would go on agreeing with
itself after that shape changed. Those tests fail rather than skip when tofu is
absent, which is what turned CI red the first time they ran there; installing it
is the fix, and making them skip would have been the fix that hides.

What keeps the two honest is that both read the version from the same place:
`go.mod` is the single pin, and this script refuses to disagree with it.

⚠️ **`scripts/check` sets `GOTOOLCHAIN=local`, and that is not a detail.**
Left unset it means `auto`, and `go run` silently downloads whatever compiler
a tool's own `go.mod` asks for — so govulncheck pinned at a version needing Go
1.26 passed on a box with 1.25 installed, scanning a standard library that was
never going to ship, while CI ran the identical command and failed because
`actions/setup-go` sets `local`. A check that passes by fetching a different
compiler is not the check CI runs.
