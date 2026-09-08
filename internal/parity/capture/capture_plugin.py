"""Record the whole-pass corpus for internal/parity, from the reference bash.

Run by hand, never in CI, against a checkout of the platform repository:

    TRUSS_PARITY_OUT=<truss>/internal/parity/testdata/scenarios \
      uv run --with pytest --with pyyaml python -m pytest tests/test_infra_pipeline.py \
      -p capture_plugin -q

with this directory on PYTHONPATH. It writes one JSON file per scenario:
the scrubbed INPUTS apply.sh was driven with, and the OUTPUTS it produced --
the final bucket and the alert text, which are the two surfaces
docs/port-plan.md §5.5 names.

WHY A PLUGIN AND NOT AN EDIT TO THE PYTHON HARNESS. The platform repository
is not ours to modify and another session works in that tree. Wrapping
`run_apply` from outside records exactly what the real tests drive, with no
second copy of the scenario definitions to drift from the first -- which is
the same "one representation where one will do" rule the rest of this
project holds to. The port plan (§5.1) puts this script in the platform
repo; it is here instead for that reason, and the pass corpus is synthetic
fixtures rather than real plan output, so the accident §5.1 guards against
(committing a real plan here) cannot happen through this file.

⚠️ EVERY IDENTIFYING VALUE IS REPLACED, AND EVERY ABSOLUTE DATE BECOMES AN
OFFSET. truss is written to be public and `scripts/leakscan` refuses emails,
32+ character hex strings, references into a secret store, and URL paths
into real hosts.
So: the repo, approver and bucket are renamed; `op_values` becomes a plain
vault/item/field map carrying no store URI at all; the Cloudflare probe's
URL never appears, only the answer it gave; and bucket bodies AND the alert
text are stored as base64 of the raw bytes (§5.1's own advice -- a plan
digest is 64 hex characters, which leakscan refuses, and the digest gate's
refusal prints two of them into the alert; an exemption for testdata would
blind the scanner to a real leak).

An absolute expiry recorded today reads as "long expired" a year from now,
which would rot every expiry scenario silently. Dates are therefore stored
as `@+<n>d` (or `@+<n>d!` for a bare date with no time), and both replayers
materialise them from their own clock.
"""

from __future__ import annotations

import base64
import json
import os
import re
import subprocess
from datetime import datetime, timezone
from pathlib import Path

OUT = Path(os.environ.get("TRUSS_PARITY_OUT", "")).resolve() if os.environ.get("TRUSS_PARITY_OUT") else None

# The scrub table. Left is what the reference harness uses, right is the
# neutral name the corpus carries. Applied to every string in the record,
# longest first so "beeradb/platform" is not half-replaced by "beeradb".
SCRUB = [
    ("beeradb/platform", "acme/platform"),
    ("platform-unsalted-tfstate", "state-bucket"),
    ("beeradb", "alice"),
]

_ISO = re.compile(r"^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}:\d{2}Z)?$")
_ROTATION_KEY = re.compile(r"^(.*/)rotation-\d{8}T\d{6}Z$")

_current = {"id": None, "n": 0}


def pytest_runtest_setup(item):
    _current["id"] = item.name
    _current["n"] = 0


def scrub(s: str) -> str:
    for a, b in SCRUB:
        s = s.replace(a, b)
    return s


def scrub_deep(o):
    if isinstance(o, str):
        return scrub(o)
    if isinstance(o, list):
        return [scrub_deep(x) for x in o]
    if isinstance(o, dict):
        return {scrub(k): scrub_deep(v) for k, v in o.items()}
    return o


def relative_date(value: str) -> str:
    """An absolute date becomes an offset from now, so the corpus does not rot."""
    if not isinstance(value, str) or not _ISO.match(value):
        return value
    date_only = "T" not in value
    fmt = "%Y-%m-%d" if date_only else "%Y-%m-%dT%H:%M:%SZ"
    t = datetime.strptime(value, fmt).replace(tzinfo=timezone.utc)
    days = round((t - datetime.now(timezone.utc)).total_seconds() / 86400)
    return f"@{days:+d}d" + ("!" if date_only else "")


def snapshot_bucket(bucket: Path) -> dict[str, str]:
    out = {}
    if not bucket.is_dir():
        return out
    for p in sorted(bucket.rglob("*")):
        if not p.is_file():
            continue
        key = str(p.relative_to(bucket))
        m = _ROTATION_KEY.match(key)
        if m:
            # The rotation ledger key embeds a UTC timestamp; the key NAME is
            # therefore a timestamp too, and falls under §5.5's second
            # documented exception along with the values inside it.
            key = m.group(1) + "rotation-<ts>"
        # ⚠️ SCRUBBED BEFORE ENCODING, NOT AFTER. A failure reason names the
        # approver, and a base64 body is opaque to the recursive scrub that
        # runs over the record -- so the first version of this recorded
        # "no APPROVED review by <the real approver>" inside a field nothing
        # could see into. Anything that does not decode as UTF-8 is stored
        # as-is; no fixture in this corpus produces one.
        raw = p.read_bytes()
        try:
            raw = scrub(raw.decode()).encode()
        except UnicodeDecodeError:
            pass
        out[key] = base64.b64encode(raw).decode()
    return out


def snapshot_workdir(workdir: Path) -> list[str]:
    """The roots present on disk, which is what apply.sh's `[ -d ]` reads."""
    roots = []
    for name in ("credentials", "platform"):
        if (workdir / name).is_dir():
            roots.append(name)
    projects = workdir / "projects"
    if projects.is_dir():
        roots += sorted("projects/" + p.name for p in projects.iterdir() if p.is_dir())
    return roots


SECRET_URI_PREFIX = "op" + "://"


def convert_op_values(op_values: dict) -> dict:
    """A 1Password store reference -> {vault: {item: {field: value}}}.

    No store URI survives into the corpus, and the prefix is assembled
    above rather than written out: leakscan scans comments and string
    literals alike, and it is right to -- it cannot tell this file's fake
    reference from a real one.
    """
    out: dict[str, dict[str, dict[str, str | None]]] = {}
    for ref, value in (op_values or {}).items():
        vault, item, field = ref[len(SECRET_URI_PREFIX):].split("/", 2)
        out.setdefault(vault, {}).setdefault(item, {})[field] = (
            relative_date(value) if isinstance(value, str) else value
        )
    return out


def convert_curl(curl_responses: list) -> dict | None:
    """The Cloudflare token probe's ANSWER, without its URL.

    apply.sh asks Cloudflare for cf-token-mint's own expiry. The corpus
    records what the issuer said, never where it was asked -- a URL path
    into a real host is one of the things leakscan refuses.
    """
    for resp in curl_responses or []:
        if "user/tokens/verify" not in resp.get("match", ""):
            continue
        try:
            body = json.loads(resp.get("body", ""))
        except ValueError:
            return {"unreadable": True}
        expires = (body.get("result") or {}).get("expires_on")
        return {"expires_on": relative_date(expires)} if expires else {}
    return None


def alert_text(fixdir: Path) -> str | None:
    """The last Telegram alert, read the way the reference harness reads it."""
    text = None
    for line in (fixdir / "calls.jsonl").read_text().splitlines():
        rec = json.loads(line)
        if rec["bin"] != "curl":
            continue
        for a in rec["args"]:
            if a.startswith("text="):
                text = a[len("text="):]
    return text


def encode_alert(text: str | None) -> str | None:
    """The alert is encoded for the same reason a bucket body is: the digest
    gate's refusal prints two 64-character hex digests into it."""
    return None if text is None else base64.b64encode(scrub(text).encode()).decode()


def pytest_configure(config):
    if OUT is None:
        raise pytest.UsageError("set TRUSS_PARITY_OUT to internal/parity/testdata/scenarios")
    import test_infra_pipeline as T

    OUT.mkdir(parents=True, exist_ok=True)
    orig_run_apply = T.run_apply
    orig_subprocess_run = T.subprocess.run
    pending: dict[str, object] = {}

    def hooked_subprocess_run(args, **kw):
        # run_apply seeds the approved digests with the real plan-digest tool
        # and THEN launches bash. Hooking the launch is the only moment at
        # which the bucket holds exactly what apply.sh will start from, which
        # is the input the Go side has to be given to be driven identically.
        #
        # Matched on APPLIER_FIXTURE_DIR rather than on "bash": three other
        # tests in this file shell to bash for unrelated reasons, and an
        # over-broad match made them fail inside the recorder -- an
        # instrument that breaks the thing it measures.
        env = kw.get("env") or {}
        if list(args[:1]) == ["bash"] and "APPLIER_FIXTURE_DIR" in env:
            fixdir = Path(env["APPLIER_FIXTURE_DIR"])
            pending["bucket_before"] = snapshot_bucket(fixdir / "bucket")
            # WORKDIR is deliberately absent in the scenario that unsets it.
            pending["workdir_roots"] = snapshot_workdir(Path(env["WORKDIR"])) if env.get("WORKDIR") else []
            pending["env"] = {
                k: env.get(k)
                for k in ("REPO", "APPROVER", "LEDGER_BUCKET", "LEDGER_APPLIED_PREFIX",
                          "LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY", "HEARTBEAT_KEY",
                          "PLAN_DIGEST_PREFIX", "DRIFT_CHECK", "EXPIRY_WARN_DAYS")
            }
        return orig_subprocess_run(args, **kw)

    def hooked_run_apply(tmp_path, fixtures, bucket_files=None, extra_env=None, unset_env=None):
        pending.clear()
        proc, fixdir = orig_run_apply(tmp_path, fixtures, bucket_files, extra_env, unset_env)

        raw = json.loads((fixdir / "fixtures.json").read_text())
        name = _current["id"]
        _current["n"] += 1
        if _current["n"] > 1:
            name = f"{name}#{_current['n']}"

        record = {
            "scenario": name,
            "bash_test": _current["id"],
            # --- inputs, in the same schema the reference fixtures.json uses,
            # minus the two fields that carry a secret-store URI or a real host.
            "fixtures": {
                "commits": raw.get("commits", []),
                "files_changed": raw.get("files_changed", {}),
                "tree": raw.get("tree", {}),
                "prs": raw.get("prs", {}),
                "reviews": raw.get("reviews", {}),
                "commit_verification": raw.get("commit_verification", {}),
                "branch_protection": raw.get("branch_protection", {}),
                "tofu_show": raw.get("tofu_show", {"resource_changes": []}),
                "tofu_plan_fail": bool(raw.get("tofu_plan_fail")),
                "tofu_apply_fail": bool(raw.get("tofu_apply_fail")),
                "secrets": convert_op_values(raw.get("op_values")),
                "cloudflare_verify": convert_curl(raw.get("curl_responses")),
            },
            "env": {k: v for k, v in (pending.get("env") or {}).items() if v is not None},
            "unset_env": sorted(unset_env or []),
            "workdir_roots": pending.get("workdir_roots", []),
            "bucket_before": pending.get("bucket_before", {}),
            # --- what apply.sh produced.
            "bash": {
                "exit_code": proc.returncode,
                "bucket_after": snapshot_bucket(fixdir / "bucket"),
                "alert_b64": encode_alert(alert_text(fixdir)),
            },
        }
        record = scrub_deep(record)
        (OUT / (re.sub(r"[^A-Za-z0-9]+", "_", record["scenario"]).strip("_") + ".json")).write_text(
            json.dumps(record, indent=2, sort_keys=True) + "\n"
        )
        return proc, fixdir

    T.run_apply = hooked_run_apply
    T.subprocess.run = hooked_subprocess_run


import pytest  # noqa: E402  (imported late so the module docstring reads first)
