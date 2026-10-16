# A router swap silently changed the wire, and the frozen test suite could not see it

**Severity:** 🔴 high · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

Nothing failed. The port from `net/http.ServeMux` to `gin.Engine` (rewrite stage P3) landed
with **all 47 `internal/api` tests and the whole `internal/httpx` suite passing unedited** —
the stage's stated completion criterion — and two real regressions on the wire:

| request | pre-Gin | first Gin port |
| --- | --- | --- |
| `GET /yeciorez/teleport` (the **documented entry URL**, and what Caddy's `handle /yeciorez/teleport*` matches) | **307 → `/yeciorez/teleport/`** (the panel) | **404 JSON** |
| `GET /yeciorez/teleport/api%2Fhealth` | **404** | **200** (it matched `/api/health`) |
| `GET /yeciorez/teleport/k/a/b` (a truncated hand-off link) | **200** (friendly "incomplete link" page) | **404** |

Plus a family of redirect losses: `//api/health` and `/api/../api/health` used to be cleaned
with a 307; `{p}/api`, `{p}/s`, `{p}/k` used to redirect to their trailing-slash forms.

## Root cause

`ServeMux` and Gin's radix tree disagree about paths in ways that no test asserted, because
the tests were **written against ServeMux**: they encode what someone thought to assert about
it, not the full semantics. Three distinct mechanisms were involved:

1. **Redirects.** `ServeMux` redirected an unclean path to its cleaned form, and the parent of
   a registered subtree pattern to the trailing-slash form — always with **307**. Gin's
   `RedirectTrailingSlash` does something similar with a **301**, which preserves nothing:
   a 301 on a POST is how a POST becomes a GET. So Gin's redirect was switched off (correctly)
   and the equivalent was simply never re-added (the bug).
2. **Subtree patterns.** `mux.HandleFunc("GET {p}/k/", h)` matched `/k/` **and everything
   beneath it**. A Gin route cannot do that next to an `:id` sibling, so `/k/a/b` fell to the
   404 fallback.
3. **Escaped vs decoded path.** `ServeMux` matched on the *escaped* path; Gin matches on the
   **decoded** one. So an encoded slash became a path separator: a request that used to be
   refused started matching a real route. This is the one that turns a 404 into a 200 — a
   refusal becoming a success, which is the wrong direction to be wrong in. It also means the
   router and `Caddy` disagree about the path of a request: Caddy sees the escaped form.

The common thread: **the change was a swap of the component that decides what a request
*is*.** Everything downstream was re-verified; the thing doing the deciding was not.

## Fix

Three additions in `internal/api/router.go`, each reproducing measured pre-Gin behaviour:

- `serveMuxRedirect` — a byte-faithful `cleanPath` (net/http's own, which unlike `path.Clean`
  preserves a trailing slash) plus an explicit list of the prefix's subtree patterns, emitting
  **307**.
- `NoRoute` delegates anything under `{p}/k/` to the hand-off handler, with an empty id — exactly
  what the old `{id}`-less subtree pattern produced.
- `rejectEncodedSlashes`, an outermost-chain middleware, answers 404 to any request whose
  escaped path contains `%2F`, so routing never sees one.

## Guard

`TestServeMuxPathSemanticsArePreserved` (`internal/api/router_test.go`): 12 cases, each value
**measured by running the pre-Gin implementation** rather than recalled, including the POST and
HEAD redirects (which is what pins 307 rather than 301) and the redirect target being reachable
(so the redirect cannot be a loop).

It was falsified: disabling the three fixes fails **11 of the 12** subtests.

## The generalisable rule

**A change to routing, framework, serialisation or the request pipeline needs a differential
run against the old implementation, not just a green suite.** The suite is evidence about the
behaviours someone previously thought about; a component swap changes behaviours nobody
enumerated. The method that found this cost one `git archive HEAD`, one probe file, and a diff
of status + headers + body over a table of path shapes — and it found what 47 passing tests
could not.

Corollary for this repository: distance between two implementations is measured, not reasoned.
The same review also caught that the ADR's own justification was **inverted** — it claimed
"ServeMux never redirected", when measurement showed ServeMux issued 307s on exactly the paths
the comment used to justify disabling Gin's redirect.
