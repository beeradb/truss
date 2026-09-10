---
name: leak-exemption
description: Narrow scripts/leakscan so it stops refusing a public identifier this repository must state. Use only when the scanner refuses something that is genuinely public — never to quiet it.
---

# Exempting something from the leak scanner

This repository is public and the infrastructure it manages is not. The
scanner refuses *classes* of identifier rather than a list of real values,
because a denylist of somebody's actual secrets would itself be the leak. Every
exemption widens a hole in that, permanently and in public.

## Before writing one

The refusal is usually correct. Ask first whether the file can simply not say
the thing: `scripts/ledger-retention` takes its bucket from the environment
precisely so this repository never writes one down. Removing the identifier
beats exempting it, every time.

## The rule

**Exempt the exact line shape, never a file, a directory or a namespace.**

- A whole file is out: a real leak added to it later is then invisible.
- A namespace is out: exempting one module path must not exempt every path
  beside it, and here the narrowness carries a second meaning — this project
  has no dependencies, so any other module path appearing in a tracked file is
  a dependency being added, and having the scanner say so is worth more than
  the convenience.
- Name the vendor or the standard, and say why the string identifies no
  deployment of ours. A vendor's documented API root is identical for every
  user; a path into a host we own is not. When the same host serves both, pin
  the exemption to the exact project path.

## What makes it real

Add **two** cases to `scripts/leakscan-test`: one proving the exempted string
now passes, and one proving a neighbouring string that is *not* public still
refuses. An exemption with only the passing half would still look correct if
the scan stopped matching altogether.

Then run `scripts/leakscan-test` before `scripts/leakscan` — the scanner's own
test answers "does this still work" before its answer is trusted — or just run
`scripts/check`, which does both in that order.

## Also refused, and not by a pattern

Commit messages naming an AI assistant. The excused SHAs live in
`scripts/leakscan-grandfathered`, each of which must still match a real commit
that still carries a trailer, so the list cannot outlive the debt it records.
Do not add to it. Write commits without the trailer.
