# P3: Gin integration strategy — Gin owns routing, the transport chain stays `http.Handler`

**Status:** 💭 PROPOSAL · **Date:** 2026-10-16 · **Deciders:** 待用户裁决（若无人反对，实施后转 ACCEPTED）
· **Supersedes:** none
· **Related:** `2026-10-16-rewrite-on-gin-and-postgres.md` (P3), `2026-10-16-p2-store-port-technique.md`

## Context

The rewrite's P3 is "`internal/httpx` → Gin 中间件；`internal/api` 路由与 handler 移植",
with the completion criterion "`internal/api` 测试全绿". The ADR's package map adds two
constraints that shape everything below:

- `internal/api/router.go` becomes a **Gin 路由表**, and handlers become **Gin `HandlerFunc`**,
  with **`{ok,data,requestId}` written by us**, never Gin's default JSON.
- `internal/httpx` middleware becomes **Gin middleware**, except `ClientIP`, which stays as is.

The current shape (measured):

| | |
| --- | --- |
| `internal/api/router.go` | 1173 lines: a `net/http.ServeMux` table (~50 routes) + ~40 handlers |
| `internal/api/keys.go` | 1123 lines of handlers |
| `internal/httpx/middleware.go` | 730 lines: RequestID, Recover, LogRequests, Compress, SecurityHeaders, CORS, ClientIP/RealIP |
| `internal/httpx/auth.go` | 487 lines: RequireAgent, RequireAgentAllowExpired, RequireSession + session crypto |
| frozen tests | `internal/api` 47 test funcs (~4100 lines) + `internal/httpx` (~1600 lines) |

Three findings from reading the code decide the trade-off:

1. **The api tests never touch the router's internals.** They build a handler with
   `New(cfg, st)` and drive it with `httptest`. So if `New` keeps the signature
   `(http.Handler, error)` — and `*gin.Engine` *is* an `http.Handler` — every one of those
   47 tests keeps compiling and asserting without an edit. That is the strongest available
   guarantee that the port does not lose a proven property.

2. **`Compress` is the subtlest code in the repository and it works by wrapping the raw
   `http.ResponseWriter`.** Its own comment documents the failure it exists to prevent: the
   header block is already flushed by the time the body arrives, so the encoding decision is
   deferred and made from metadata, and getting it wrong produces `Content-Length: 107422`
   with no `Content-Encoding`, gzip bytes on the wire, and a client that aborts with "end of
   response with N bytes missing". Gin's `c.Writer` is **not** the raw writer, so re-expressing
   this against Gin means rebuilding that delicate writer and re-earning the confidence.
   `CORS` likewise must see `OPTIONS` **before** routing to answer 204 (a frozen test,
   `TestCORSPreflightOnRealRoute`, asserts it).

3. **Exactly one route needs a real 405, and it is frozen.** `HEAD` on
   `GET /api/agent-keys/applications/{id}` must answer 405 with `Allow: GET`
   (`TestKeyHandoffClaimRefusesHEAD`): a HEAD has no body to carry the credential back, so
   letting it reach the claim handler would burn a one-time link. Today this works because
   `net/http`'s `ServeMux` serves HEAD from a GET pattern and the handler guards itself.
   **Gin does not**: HEAD does not match a GET route, so this contract breaks silently unless
   it is handled deliberately.

## Options

| Option | Upside | Downside |
| --- | --- | --- |
| **A. Gin for routing/dispatch; transport middleware stays `http.Handler` around the engine** | The compression writer, CORS-before-routing and the whole outer chain are preserved *by construction*, and their tests stay meaningful. `New` keeps its signature so all 47 api tests stay frozen. Gin genuinely owns what the ADR asks it to own: the route table, method matching, params (`c.Param`) and the 404/JSON fallback. | Not a "pure" Gin stack: middleware registered with `Use` is absent, and the chain is composed by `httpx.Chain` rather than Gin. A reader expecting idiomatic Gin needs the reason written down. |
| **B. Everything in Gin, including middleware** | Idiomatic: `engine.Use(...)`, middleware as `gin.HandlerFunc`, no net/http middleware left. | Requires re-deriving `compressWriter` against `c.Writer` (finding 2) and moving CORS inside routing, where OPTIONS must still win before a 404/405 — re-earning two delicate behaviours for zero wire-visible gain. Also forces rewriting ~1600 lines of httpx tests, replacing frozen evidence with newly written evidence. |
| **C. `gin.WrapH` for every route; handler bodies untouched** | Smallest diff by far: the route table moves, handler bodies and all tests stay. | Handlers are not `HandlerFunc`, so `c.Param` is unused and the ADR's package map is only half honoured. Reads as a routing swap rather than a port. |

## Decision

**Option A**, with the handler/dialect work of the ADR done in full. Concretely:

- `New` still returns `(http.Handler, error)`; it returns a `*gin.Engine`.
- The engine's conveniences are all off: `RedirectTrailingSlash=false`,
  `RedirectFixedPath=false`, `HandleMethodNotAllowed=false`. Gin's own redirect emits a
  **301**, and a 301 on a POST is how a POST silently becomes a GET; `ServeMux` redirected
  the same paths with a **307**, which preserves the method. So rather than accept Gin's
  status, `serveMuxRedirect` in NoRoute reproduces the redirect with the correct one.
  `HandleMethodNotAllowed=false` keeps a wrong verb a 404 with the JSON envelope, which is
  what the per-prefix catch-alls used to produce and what `TestMethodNotAllowed` asserts.
- Gin owns the route table; every route registered with `GET` is **also registered for
  `HEAD`**, because that is exactly what `ServeMux` did. Without it the two HEAD guards in
  `keys.go` become dead code and the frozen 405 contract breaks.
- Handlers become `gin.HandlerFunc` using `c.Param`, per the ADR.
- The envelope helpers (`OK`, `Fail`, `WriteError`, `BadRequest`, …) keep their
  `(http.ResponseWriter, *http.Request, ...)` signatures and are called with
  `c.Writer, c.Request`. That keeps the envelope byte-identical — including
  `SetEscapeHTML(false)` and the `requestId` field — and keeps `errors.go`'s tests frozen.
- Auth middleware is applied per-route as Gin middleware, but its **core stays in one place**
  and the principal/session are still delivered through the request context
  (`c.Request = c.Request.WithContext(...)`), so `httpx.PrincipalFrom(c.Request.Context())`
  and the server-rendered pages keep working unchanged.
- The outer transport chain is preserved — `httpx.Chain(engine, Recover, RequestID,
  LogRequests, SecurityHeaders, CORS(...), rejectEncodedSlashes, Compress)` — with exactly one
  addition: `rejectEncodedSlashes`, which must run *before* routing to undo Gin's decoding of
  `%2F` (see the divergence section below). Everything else in the chain is the same middleware
  in the same order, which is the point: the compression writer and the CORS preflight are
  preserved by construction rather than re-derived.
- `NoRoute` answers the JSON 404 envelope, which is what the `/api/`, `/s/` and prefix
  catch-alls did.
- `spa.Handler` is *not* wrapped in a route. It is called from `NoRoute` as `static.ServeHTTP`,
  because the SPA is the fallback for every path the router did not match — which is also what
  it was before (`mux.Handle(p+"/", static)`). It stays a plain `http.Handler`; there is nothing
  to gain by rewriting it, and mounting it through `gin.WrapH` on a catch-all route would change
  which paths reach it.

### Why not B, stated plainly

B buys idiomatic middleware registration and pays with two behaviours whose failure modes are
subtle, silent and already documented in this repository — and the only observable difference is
internal. The port's contract is that the wire and the frozen contracts do not change; the
transport chain is the part of the system where "it looks cleaner now" is most expensive.

## Consequences

- **Easier:** the outer chain and its tests are untouched; the api tests stay frozen; the risk is
  concentrated in the route table, which is where the ADR wants the change anyway.
- **Harder:** a reader must know that `Use` is deliberately not used for the transport chain.
  This file is that record, and `New`'s comments repeat it at the point of confusion.
- **Explicitly kept:** `RealIP` stays display-only and `ClientIP` stays the only security-relevant
  source of client address (a frozen contract; rate-limit buckets must keep using it through a
  Gin handler that reaches for `c.Request`).
- **New obligation:** `gin` enters `go.mod`. It is the only new direct dependency.

## Gin version: pinned to v1.10.1, and why

Measured while porting, and it changed the choice:

| gin | packages linked into the binary from `mongo`/`quic` | total linked packages | binary |
| --- | --- | --- | --- |
| v1.12.0 | 22 | 349 | 34.0 MB |
| **v1.10.1** | **0** | **339** | **33.1 MB** |
| v1.11.0 | 15 | 349 | 33.3 MB |

From v1.11 onward, `gin/binding` pulls `go.mongodb.org/mongo-driver/v2/bson` and the `gin`
package pulls `github.com/quic-go/quic-go/http3` — so a service that hand-writes its JSON
envelope, parses its own bodies, and speaks only HTTP/1.1 behind Caddy was about to link in a
MongoDB BSON codec and an HTTP/3 stack. `go mod why -m` confirms the chain:
`internal/api -> gin -> gin/binding -> mongo-driver/v2/bson`, and
`internal/api -> gin -> quic-go/http3`.

v1.10.1 is therefore pinned: the newest Gin that does not carry either. This is a
supply-chain and binary-size decision, not a feature one — nothing this service uses
differs between them (no Gin binding, no Gin render, no HTTP/3). Revisit only with a
measured reason; "newer is better" is how the BSON driver got in.

## Verification recorded with this decision

- `internal/api` (47 test functions) and `internal/httpx` pass **without a single test edit**,
  which is the ADR's completion criterion and the strongest available evidence that no proven
  behaviour moved.
- The three behaviours that break under Gin's defaults were each confirmed by running the frozen
  test that covers them: `TestMethodNotAllowed`, `TestKeyHandoffClaimRefusesHEAD`,
  `TestCORSPreflightOnRealRoute`, plus `TestUnknownRouteWithSPAStaysJSON`,
  `TestNewRoutesAreNotSwallowedBySPA` and `TestRoutesAreMountedUnderPrefix`.
- **Route-table fidelity was checked mechanically, not by eye.** Parsing both tables and
  normalising `{id}`/`:id` gives: every one of the **40** ServeMux method+path entries survives,
  and the only additions are **HEAD twins for all 20 GET routes** — no route missing, no method
  dropped, no typo (a typo in a route string is a silent 404).
- **Gin adds no response headers.** A probe of every header on `/api/health` returned only ours:
  the four security headers, `Content-Type`, `X-Request-Id`. No `Server`, no `X-Powered-By`.
- **Compression still works through the engine** (the risk this design was chosen to avoid): a
  gzip request for `/llms.txt` returns `Content-Encoding: gzip`, drops `Content-Length`, sets
  `Vary: Accept-Encoding`, and inflates to **exactly** the bytes the uncompressed request
  returned (3289 → 1516). Made permanent as `TestCompressWorksThroughGinEngine`.
- **`c.Abort()` really stops the chain**: a bad-credential `POST /api/reports` returns 401 and
  creates **zero** rows. Made permanent as `TestRefusedRequestNeverReachesTheHandler`, which
  asserts the effect (row count) rather than the status — a 401 alone would look correct even if
  the handler had already written.
- Two new guards, each falsified rather than assumed: dropping `http.MethodHead` from the `get`
  helper makes `TestHeadIsRoutedLikeGet` **and** the frozen `TestKeyHandoffClaimRefusesHEAD` fail
  (405 degrades to 404, exactly as predicted).

## Divergences found by differential review, and fixed

The first pass treated "47 frozen tests green" as sufficient evidence. A differential
review disagreed, correctly: it built the pre-Gin tree (`git archive HEAD`) and the ported
tree, drove **57 requests × 2 fixtures = 114** through both, and diffed status, 15 headers
and body. **12 of 57 cases differed in each fixture, and no frozen test covered any of
them.** Three were real, and all three are now fixed and pinned by
`TestServeMuxPathSemanticsArePreserved` (12 subtests, each value measured against the old
tree, and falsified — disabling the three fixes fails 11 of the 12).

| request | pre-Gin | first Gin port | now |
| --- | --- | --- | --- |
| `GET {p}` (bare prefix) | 307 → `{p}/` | **404 JSON** | 307 → `{p}/` |
| `GET {p}//api/health`, `{p}/api/../api/health` | 307 → cleaned | 404 | 307 → cleaned |
| `GET {p}/api`, `{p}/s`, `{p}/k` | 307 → trailing slash | 404 JSON | 307 → trailing slash |
| `GET {p}/api%2Fhealth` | 404 | **200** (matched `/api/health`) | 404 |
| `GET {p}/k/a/b` (truncated link) | 200 (subtree) | 404 | 200 |

- The bare prefix is the one with a user-visible effect: it is the **documented entry URL**,
  and hcyj's Caddy uses `handle /yeciorez/teleport*`, which matches the no-slash form. The
  panel's front door was answering a JSON 404.
- `%2F` was the dangerous one: Gin matches on the **decoded** path where ServeMux matched on
  the escaped one, so a request that used to be refused started succeeding. Refusals must not
  become successes; `rejectEncodedSlashes` now refuses it before routing sees it.
- `/k/` needed the subtree because a Gin route cannot carry both `:id` and a catch-all at the
  same position. NoRoute delegates, and the handler reads an empty id exactly as the old
  `{id}`-less subtree pattern did.

**This is the general lesson of P3, and it is why the ADR's own verification section was
wrong the first time:** the frozen tests were written against ServeMux, so they encode only
what someone thought to assert about ServeMux. A router swap is exactly the change where
"the suite is green" and "the behaviour is the same" come apart, and only a side-by-side
differential run against the old implementation can close that gap. It should be the standard
move for the remaining framework changes (P3 is the last of them, but the same applies to any
future router, middleware or serialisation swap).

## Revisit when

- Gin's `c.Writer` grows a documented, tested equivalent for deferred header decisions and
  someone is willing to re-verify the compression matrix — then the transport chain can move
  inside Gin.
- A future endpoint needs Gin-native streaming or SSE, where the outer-compression opt-out
  (`Flush`) may need revisiting.
