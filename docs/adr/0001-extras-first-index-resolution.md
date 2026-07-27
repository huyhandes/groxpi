# ADR 0001: Extra indexes are resolved first, and index results are never merged

- Status: accepted
- Date: 2026-07-27
- Issues: #3, #22, #23

## Context

groxpi can be configured with a primary index (`GROXPI_INDEX_URL`) and any number
of extra indexes (`GROXPI_EXTRA_INDEX_URLS`). Organisations run an extra index
because they publish internal packages to it. Until this change the extra list was
parsed and never queried, so a decision about resolution order had to be made
before the feature could work at all.

The obvious implementation — ask every index and merge the file lists — is the
dependency-confusion attack. An attacker registers a package on PyPI under an
internal name; a merging proxy lists the attacker's files alongside the genuine
ones, the resolver picks by version, and because a caching proxy caches the merged
result the poisoned list is then served to the whole organisation. "Merge but
prefer the private index" has the same hole: the attacker only needs one file the
private index does not have.

## Decision

1. **Extras first, primary last.** Extra indexes are consulted in configured
   order; the primary index is consulted last.
2. **First hit wins, whole.** The first index that returns the package supplies
   the file list unmodified. There is no merging, no union and no deduplication
   across indexes.
3. **Not-found needs every index.** A 404 is returned only after every configured
   index has been consulted and missed.
4. **A failure is not a miss.** If a higher-priority index errors (connection
   refused, 5xx), resolution fails instead of falling through to a lower-priority
   index. Falling through would mean an outage on the private index re-opens the
   shadowing hole for exactly as long as the outage lasts.
5. **Concurrent across extras, sequential before the primary.** The extras are
   queried in parallel, but selection walks the configured order and waits for
   each index in turn, so arrival order never decides — a fast index cannot answer
   for a name a slower, higher-priority index also has. Lower-priority queries
   still in flight are cancelled once an index has answered. The primary is
   deliberately *not* queried speculatively: sending PyPI a request for
   `acme-internal-utils` leaks the internal package name even if the response is
   thrown away.
6. **Per-index TTL.** The cache entry is stored with the answering index's TTL and
   records which index answered, so a fast-moving private index is not held stale
   by PyPI's schedule.
7. **Credentials in the URL are redacted at every rendering site.** `Index.Redacted()`
   / `config.RedactURL` replace the user-info with a fixed placeholder, and the raw
   URL is never formatted into a log field, an error or a response body. Transport
   errors get special handling because `*url.Error` prints the URL it failed on.

## Consequences

- A package that genuinely exists on both a private index and PyPI **always**
  resolves to the private one, even when PyPI has a newer version. This is
  intended. **Do not "fix" it by merging** — merging is the attack.
- Shadowing an internal name on a private index is a shadow of the public package
  for groxpi's clients. That is the same trust boundary an operator already accepts
  by running the private index.
- Resolution of a public package costs the extras' latency before the primary is
  asked. Extras are queried in parallel, so the cost is the slowest extra, not
  their sum.
- An unreachable extra index breaks resolution of *all* packages rather than
  quietly degrading to PyPI. This is the deliberate fail-closed choice in point 4;
  an operator who wants availability over the guarantee has to remove the index
  from the list.

## Alternatives rejected

- **Merge all results.** The attack.
- **Merge, prefer private per file.** The attack with extra steps.
- **First-to-respond wins.** Turns resolution into a race the attacker can win by
  being faster than the private index.
- **Query all indexes concurrently, including the primary.** Slightly lower
  latency for public packages, at the price of leaking every internal package name
  to PyPI.
- **Fall through to the primary when a higher-priority index fails.** Better
  availability, but makes the guarantee conditional on the private index being up.

## Upgrade impact (release note)

Anyone who already had `GROXPI_EXTRA_INDEX_URLS` configured was getting nothing
from it: the setting was parsed and ignored. After this change those indexes are
queried, and queried *before* PyPI. A package that resolved from PyPI yesterday
can resolve from an extra index today. That is the point of the feature, but it is
a behavioural change on upgrade. Operators who listed an index they did not
actually want in the resolution path should remove it before upgrading.
