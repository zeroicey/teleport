# A verification script that cannot fail is worse than no verification

**Severity:** 🟠 medium · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

Stage P4's whole purpose was to prove the assembled binary works end to end. The first
run reported **23 passed, 32 failed** — and almost every failure was in the verification
script, not the product. The reverse failure mode is worse and harder to notice: several
checks reported **PASS while asserting nothing**, so the first "64/64 green" was partly
theatre.

Concrete examples, all found in one afternoon:

| Check | What it actually did |
| --- | --- |
| `the guide links the live base URL` | Compared against a variable that was empty at that point, so `grep -q ""` matched everything |
| `the guide's URLs are rewritten (no unresolved placeholders)` | Polarity inverted — it passed when placeholders **were** present |
| `the session cookie is set under its frozen name` | `Network.getCookies` queried at the origin root, but the cookie is scoped to the route prefix; "(none)" was indistinguishable from "not set" |
| `the injected payload is not emitted as markup` | Asserted the literal text `raw HTML omitted`, but the contract is only that raw HTML is *absent* — a custom renderer's placeholder text is not the contract |
| `Mermaid rendered to SVG` | Asserted Mermaid's `data-processed` attribute, which v12 does not set; the SVG was rendered correctly and the check still failed |

## Root cause

Two mechanisms, both about **who wrote the check and who wrote the code**:

1. **The same author wrote both, so a shared misunderstanding passes silently.** The
   `data-processed` case is the clearest: the check encoded a belief about Mermaid's DOM
   that happened to be false, and the assertion failed on a *working* diagram. The
   `raw HTML omitted` case is the mirror image: the check encoded an implementation
   detail as if it were the contract.

2. **Shell string handling fails quietly.** Three separate bugs came from quoting:
   - a header value passed as a bare positional argument, so `curl` read
     `Authorization: Bearer x` as a **URL** and answered `URL rejected: Malformed input`;
   - `\`` inside single quotes inside `$(printf ...)`, which produces a literal backslash —
     so the seeded report contained `` \`\`\`mermaid `` instead of a real fence, the page
     had no diagram, and the browser check failed for a reason with nothing to do with
     the product;
   - `export X="$(cmd)"`, which masks the command's exit status, so a failed secret
     generation would have started the server with an empty secret.

   None of these is visible by reading the script. Each was found only by running it and
   disbelieving the result.

A third, subtler one: **`hash-password` prints a ready-to-paste env line**
(`ADMIN_PASSWORD_HASH=pbkdf2$...`), so using its stdout as the env value embedded the
literal prefix and every login returned 401 — 20 cascading failures from one character.

## Fix

- Assertions were rewritten to test **the property**, not a proxy: "no `<script>` element
  exists" rather than "this placeholder text exists"; "an `<svg>` exists inside a
  `.mermaid` element" rather than "Mermaid set this attribute".
- `req()` now builds its whole `curl` argv from typed parameters
  (`req <method> <url> <auth-header> [json-body]`), so a header cannot be passed without
  its `-H` and a body cannot be passed in the wrong position.
- Transport failure sets `CODE=000`, never empty — an empty `CODE` silently fails every
  `[ "$CODE" = 404 ]` comparison and makes the failure message say nothing.
- Cookie lookups use a URL **under** the prefix.
- `hash-password` output is stripped **and the strip is asserted**, so a format change
  fails with a clear message instead of as 20 mystery 401s.
- JSON bodies that contain code fences are generated in Python, not by shell escaping.

## Guard

The guards are in the scripts themselves, and each was **falsified**:

- Running the contract script against a server built **without** the embedded frontend
  makes `the prefix root serves the SPA shell` and `the shell references a hashed asset`
  fail — proving those two checks are not vacuous.
- The new pre-flight (`something is already serving …`) was falsified by leaving a stale
  `go run .` on the port: it refuses with a clear message instead of running the checks
  against the wrong process. That stale server was the most likely cause of a single
  unexplained check failure observed once in five runs — and it is exactly the class of
  problem that makes a verification untrustworthy.
- Contract and browser output is now kept at `bin/e2e/p4-{contract,browser}.log`, so a
  failure that appears once in several runs is diagnosable afterwards. A green run's
  output is uninteresting; a one-in-five failure is impossible to debug from a scrolled
  terminal.

## The generalisable rule

**A verification must be falsified before it is believed.** The P2.3 and P3 rounds
established that for *code* guards (mutation-test them). P4 shows the same applies to the
*verification harness*: run it against a deliberately broken input — no frontend, a stale
server, an empty variable — and watch it fail. If it cannot be made to fail, it is not
evidence.

Corollary: when a check fails, the first question is "is the check wrong?", not "is the
product wrong?". In this session the check was wrong far more often than the product was.
