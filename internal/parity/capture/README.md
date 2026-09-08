# Recording the whole-pass parity corpus

`capture_plugin.py` records the corpus that `internal/parity` replays. It runs
**by hand, never in CI**, against a checkout of the reference (platform)
repository, and it does not modify that repository: it is a pytest plugin that
wraps the reference harness's own `run_apply` from outside.

```
TRUSS_PARITY_OUT=<truss>/internal/parity/testdata/scenarios \
PYTHONPATH=<truss>/internal/parity/capture:tests \
  uv run --with pytest --with pyyaml \
  python -m pytest tests/test_infra_pipeline.py -q -p capture_plugin
```

run from the root of the platform checkout. Some of that suite's tests fail for
reasons of their own (its workflow files, its commit hook); the recorder does
not care, and every test that drives `apply.sh` still records.

The same thing, checked rather than trusted, from the truss side:

```
TRUSS_PARITY_BASH=1 TRUSS_PLATFORM_REPO=/path/to/platform \
  go test ./internal/parity -run TestRecordedCorpusMatchesTheLiveBash
```

## What it writes, and why it is shaped that way

One JSON file per scenario: the inputs `apply.sh` was driven with, and the two
outputs §5.5 compares — the final bucket, key by key, and the alert text.

Three transformations happen on the way out, each because
`scripts/leakscan` would refuse the file otherwise, and an exemption for a
testdata directory would blind that scanner over everything in it:

- **Identifying names are replaced.** The repository, the approver and the
  bucket become neutral ones.
- **Bucket bodies and the alert are base64 of the raw bytes.** A plan digest is
  64 hex characters, and the digest gate's refusal prints two of them into the
  alert.
- **The 1Password references and the Cloudflare probe's URL are converted into
  structure.** A store URI becomes a vault/item/field map; the probe's URL
  becomes the answer it gave.

And one because a corpus has to keep meaning what it meant:

- **Absolute dates become offsets** (`@+300d`, or `@-2d!` for a bare date).
  Every expiry fixture in the reference suite is written as "now plus n days".
  Frozen as an instant, a scenario recording "300 days left" would read as long
  expired a year from now and would quietly stop testing what it was written
  to test. Both replayers materialise these from their own clock; the day
  counts are therefore compared with a one-day tolerance rather than exactly.

## Where this lives, and why it is not in the platform repository

`docs/port-plan.md` §5.1 puts the capture script in the platform repository, so
that no real plan output can be committed to truss by accident. That reasoning
is about the **digest** corpus, which is captured from real `tofu show -json`
output. This is the **pass** corpus: synthetic fixtures the reference test file
already builds, with no real plan in them, so the accident §5.1 guards against
cannot happen through this file. It lives here because the platform repository
is read-only to this work, and because a recorder that wraps the reference
harness from outside has no second copy of the scenario definitions to drift
from the first.
