# 0003: The Files section is an additive body section under aj-episode.v1

Date: 2026-09-03
Status: accepted

## Context

Episodes record which tools a turn used but not which files it read, so the
estate's introspect sweep cannot see model-invoked skill or note
consultation. Adding a `## Files` body section is new grammar for the
rendered episode, and the corpus-durable tier says episode bytes, identity,
and digest derivation change only at a major version. The choice was between
a new schema tag with a major release, a frontmatter encoding that avoids
body grammar, and an additive section under the existing tag.

## Decision

`## Files` is an optional body section after `## Tools`, rendered only when
the turn consulted something, and covered by the payload digest only when
present. `aj-episode.v1` and `autojournal-digest.v1` keep their names and the
change ships as a minor version.

## Consequences

Every existing episode keeps its bytes, id, and digest, so no evidence
reference is invalidated and the golden pins stay in force. The durability
promise (a future build reads every past corpus) holds; the reverse does
not: a 2.0 build reports a Files-bearing episode as digest-mismatch. This
estate is the engine's only consumer, so that is documented, not versioned.
A later section that must appear on every episode, or one that changes the
meaning of existing bytes, does not get this treatment and takes the major.

## Considered options

- **`aj-episode.v2` and a 3.0 release**: honest about the grammar, but a
  second golden fixture set and a major for a change no existing episode
  feels.
- **Frontmatter keys**: the frontmatter parser is equally closed, and a
  list of paths as repeated single-line keys reads badly and hits the same
  unknown-key rejection.
