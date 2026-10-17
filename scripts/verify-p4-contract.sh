#!/usr/bin/env bash
#
# P4 end-to-end contract check: drive a REAL running binary over HTTP and assert the
# frozen contracts that the Go tests can only assert in-process.
#
# Why this exists when the suite already covers them: the Go tests call the handler
# directly through httptest. They prove the handlers' logic. They do not prove that the
# assembled binary — Gin routing, the transport middleware chain, the embedded SPA and
# the real PostgreSQL — answers a real client over a real socket the same way. P4 is
# the stage that says "the whole chain works", and this is the script that checks it.
#
# Usage:
#   scripts/verify-p4-contract.sh <base-url> <route-prefix> <agent-secret> <admin-password>
#
# Prints PASS/FAIL per contract and exits non-zero if any check fails, so a failure
# names the contract it broke instead of "something went wrong".
set -uo pipefail

# The prefix is REQUIRED as an argument but may be EMPTY: the P6 deployment serves
# the root path, and `"${2:?}"` would treat an explicit empty value as "missing"
# and refuse to run. Distinguishing "no argument" from "empty argument" is the
# whole point here — the same distinction the ROUTE_PREFIX env reader gets wrong
# for empty strings (see .ai/runbooks/p6-cutover.md risk 13).
if [ $# -lt 2 ]; then
  echo "usage: $0 BASE PREFIX AGENT_SECRET ADMIN_PASSWORD   (PREFIX may be empty for a root-path deployment)" >&2
  exit 2
fi
BASE="$1"
P="$2"
AGENT="${3:?agent secret (AGENT_SECRET_KEY) required}"
ADMIN_PASSWORD="${4:?admin password required}"

API="$BASE$P"
JAR="$(mktemp)"
trap 'rm -f "$JAR"' EXIT

REQ_EXTRA=()
pass=0
fail=0
# grep_q / grep_qF / grep_qE — grep a large string for a pattern WITHOUT a pipe.
#
# `printf '%s' "$BIG" | grep -q PATTERN` is a trap under `set -o pipefail`: grep -q
# exits at the FIRST match, the writer then takes SIGPIPE, and the pipeline's status
# becomes non-zero even though the pattern matched. Measured during the P6 cut-over:
# the AI guide contained its base URL 13 times and the check still reported FAIL,
# intermittently (~1 run in 10), because whether the writer is still flushing when
# the reader exits depends on timing. The pipe buffer is 4 KB and the guide is 39 KB.
#
# It is worse in the other direction: for a check phrased "must NOT contain X", the
# same SIGPIPE makes the pipeline non-zero exactly when X IS present, so the check
# silently PASSES. Those are the XSS and placeholder-leak checks.
#
# Handing grep a FILE removes the race: the reader never closes the writer's pipe.
grep_q() { # <text> <pattern> [grep flags...]
  local text="$1" pattern="$2"; shift 2
  local f rc
  f="$(mktemp)" || return 2
  printf '%s' "$text" > "$f"
  grep -q "$@" -- "$pattern" "$f"
  rc=$?
  rm -f "$f"
  return $rc
}
grep_qF() { local t="$1" p="$2"; grep_q "$t" "$p" -F; }
grep_qE() { local t="$1" p="$2"; grep_q "$t" "$p" -E; }

check() { # name, ok(0=pass), detail
  if [ "$2" = "0" ]; then
    printf 'PASS  %s\n' "$1"; pass=$((pass + 1))
  else
    printf 'FAIL  %s  — %s\n' "$1" "${3:-}"; fail=$((fail + 1))
  fi
}

# jget <json> <python expr on d>
jget() {
  python3 -c '
import json,sys
try:
    d=json.loads(sys.argv[1])
    v=eval(sys.argv[2])
except Exception:
    v=None
print("" if v is None else v)
' "$1" "$2" 2>/dev/null
}

# req <method> <url> <auth-header-or-empty> [json-body]  -> sets CODE, BODY
#
# The auth value is a HEADER LINE ("Authorization: Bearer x", "Cookie: a=b") and is
# passed to curl behind an explicit -H. An earlier version of this script took the
# header as a trailing positional argument and forgot the -H, so curl read
# "Authorization: Bearer ..." as a URL and answered "URL rejected: Malformed input".
# Building the whole argv here makes that mistake impossible.
#
# The body is a separate, explicitly typed argument, so no call site can silently
# supply it in the wrong position.
#
# A transport failure sets CODE=000 (never empty): an empty CODE silently fails every
# `[ "$CODE" = 404 ]` comparison, so a broken call would read as "not 404" and the
# failure detail would say nothing about why.
req() {
  local method="$1" url="$2" auth="${3:-}" data="${4:-}"
  if [ -z "$url" ]; then
    CODE="000"; BODY="(empty url — a variable the url depended on was empty)"
    return
  fi
  local args=(-sS -X "$method" -w '\n%{http_code}')
  [ -n "$auth" ] && args+=(-H "$auth")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "$data")
  fi
  if [ "${#REQ_EXTRA[@]}" -gt 0 ]; then
    args+=("${REQ_EXTRA[@]}")
  fi
  local out
  if ! out=$(curl "${args[@]}" "$url" 2>&1); then
    CODE="000"; BODY="$out (from $method $url)"
    return
  fi
  CODE=$(printf '%s' "$out" | tail -n1)
  BODY=$(printf '%s' "$out" | sed '$d')
}

# head_probe <url> <auth-header-or-empty> -> sets CODE (status) and ALLOW
head_probe() {
  local url="$1" auth="${2:-}"
  local args=(-sSI)
  [ -n "$auth" ] && args+=(-H "$auth")
  local out
  out=$(curl "${args[@]}" "$url" 2>&1)
  CODE=$(printf '%s' "$out" | head -1 | tr -d '\r' | awk '{print $2}')
  ALLOW=$(printf '%s' "$out" | tr -d '\r' | awk 'tolower($1)=="allow:"{print $2}')
  BODY="$out"
}

AGENT_H="Authorization: Bearer $AGENT"
STAMP="p4-$(date +%s)-$$"
APP_ID_RE='id=["'"'"']?app["'"'"']?'

echo "==> 1. envelope, health, and the frozen prefix"
req GET "$API/api/health" ""
check "GET $P/api/health is 200" "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE"
check "the success envelope is {ok:true,data,requestId}" \
  "$([ "$(jget "$BODY" 'd["ok"]')" = True ] && [ -n "$(jget "$BODY" 'd["data"]')" ] && [ -n "$(jget "$BODY" 'd["requestId"]')" ] && echo 0 || echo 1)" "$BODY"

if [ -n "$P" ]; then
  # Non-empty prefix: it must be genuinely enforced, so the unprefixed path is a
  # different path and must 404.
  req GET "$BASE/api/health" ""
  check "the same path WITHOUT the prefix is not served (404)" \
    "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE"
else
  # Root-path deployment (P6). There is no "without the prefix" variant — $API and
  # $BASE are the same URL — so that check would contradict the 200 asserted above.
  # The analogous property is that the OLD prefix is not serving the API either.
  #
  # Measured against a bare backend (no Caddy): /yeciorez/teleport/api/health falls
  # through to the SPA fallback — 200 text/html, NOT the JSON envelope. That is
  # acceptable only because Caddy answers the old prefix with 410 before the request
  # ever arrives, and it is why that Caddy rule is REQUIRED rather than cosmetic:
  # without it an old link would receive a 200 HTML shell whose asset URLs point at
  # the root of api.hcyj.xyz, which is a different service.
  req GET "$BASE/yeciorez/teleport/api/health" ""
  check "the OLD prefix is not serving the API (no JSON envelope)" \
    "$([ "$CODE" = 200 ] && grep_qF "$BODY" '"ok":true' && echo 1 || echo 0)" \
    "got $CODE ${BODY:0:60}"
fi

req GET "$API/api/reports" ""
check "an unauthenticated call is 401 with {ok:false,error:{code,message},requestId}" \
  "$([ "$CODE" = 401 ] && [ "$(jget "$BODY" 'd["ok"]')" = False ] && [ "$(jget "$BODY" 'd["error"]["code"]')" = unauthorized ] && [ -n "$(jget "$BODY" 'd["requestId"]')" ] && echo 0 || echo 1)" "got $CODE $BODY"

req GET "$API/api/definitely-not-a-route" "$AGENT_H"
check "an unknown /api/* path is still JSON, not the SPA's HTML" \
  "$([ "$CODE" = 404 ] && grep_qF "$BODY" '"ok"' && echo 0 || echo 1)" "got $CODE ${BODY:0:80}"

echo
echo "==> 2. create a report with a real credential (request camelCase -> response snake_case)"
req POST "$API/api/reports" "$AGENT_H" \
  "{\"title\":\"$STAMP markdown\",\"content\":\"# hello\n\n<script>alert(1)</script>\n\",\"category\":\"pentest\",\"autoShareHours\":0}"
check "POST /api/reports is 201" "$([ "$CODE" = 201 ] && echo 0 || echo 1)" "got $CODE $BODY"
RID=$(jget "$BODY" 'd["data"]["id"]')
TOK=$(jget "$BODY" 'd["data"]["share"]["token"]')
check "the response is snake_case and carries a share token" \
  "$([ -n "$RID" ] && [ -n "$TOK" ] && echo 0 || echo 1)" "$BODY"
check "created_at is epoch milliseconds" \
  "$(printf '%s' "$(jget "$BODY" 'd["data"]["created_at"]')" | grep -qE '^[0-9]{13}$' && echo 0 || echo 1)" "got $(jget "$BODY" 'd["data"]["created_at"]')"
check "autoShareHours:0 means never expires (expires_at = 0)" \
  "$([ "$(jget "$BODY" 'd["data"]["share"]["expires_at"]')" = 0 ] && echo 0 || echo 1)" "got $(jget "$BODY" 'd["data"]["share"]["expires_at"]')"
check "share.url is <prefix>/s/<token>" \
  "$([ "$(jget "$BODY" 'd["data"]["share"]["url"]')" = "$API/s/$TOK" ] && echo 0 || echo 1)" "got $(jget "$BODY" 'd["data"]["share"]["url"]')"

req PATCH "$API/api/reports/$RID" "$AGENT_H" '{"title":"hijacked","nonsense":true}'
check "an unknown body field is 400 bad_request" \
  "$([ "$CODE" = 400 ] && [ "$(jget "$BODY" 'd["error"]["code"]')" = bad_request ] && echo 0 || echo 1)" "got $CODE $BODY"
req GET "$API/api/reports/$RID" "$AGENT_H"
check "a rejected PATCH wrote nothing" \
  "$([ "$(jget "$BODY" 'd["data"]["title"]')" = "$STAMP markdown" ] && echo 0 || echo 1)" "got $(jget "$BODY" 'd["data"]["title"]')"
req PATCH "$API/api/reports/$RID" "$AGENT_H" '{"title":"'"$STAMP"' patched"}'
check "a valid PATCH is 200 and advances updated_at past created_at" \
  "$([ "$CODE" = 200 ] && [ "$(jget "$BODY" 'd["data"]["updated_at"]')" -gt "$(jget "$BODY" 'd["data"]["created_at"]')" ] && echo 0 || echo 1)" "got $CODE $BODY"

echo
echo "==> 3. markdown never renders raw HTML"
PAGE=$(curl -sS "$API/s/$TOK")
check "the share page contains no live <script>alert(1)</script>" \
  "$(grep_qF "$PAGE" '<script>alert(1)</script>' && echo 1 || echo 0)"
# The contract is that raw HTML is never rendered — not that any particular
# placeholder appears. The frozen markdown tests assert exactly this way (they ban
# `<script>` and the payload); the renderer is custom, so its placeholder text is an
# implementation detail and asserting it would pin the wrong thing.
check "the injected payload is not emitted as markup" \
  "$(grep_qE "$PAGE" '<[^>]*alert\(1\)' && echo 1 || echo 0)"
check "the report's own markdown content IS rendered" \
  "$(grep_qF "$PAGE" 'hello' && echo 0 || echo 1)"

echo
echo "==> 4. dashboard session, and link semantics (unknown 404 / revoked 404 / expired 410)"
REQ_EXTRA=(-c "$JAR")
req POST "$API/api/admin/login" "" "{\"password\":\"$ADMIN_PASSWORD\"}"
REQ_EXTRA=()
check "the dashboard logs in and sets a session cookie" \
  "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE $BODY"
SESSION="$(awk '/teleport_session/{v=$7} END{print v}' "$JAR")"
check "the session cookie is named teleport_session (a frozen contract)" \
  "$([ -n "$SESSION" ] && echo 0 || echo 1)" "jar=$(tr '\n' ' ' < "$JAR" | head -c 200)"
COOKIE_H="Cookie: teleport_session=$SESSION"
req GET "$API/api/admin/session" "$COOKIE_H"
check "the session authenticates a dashboard call" "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE $BODY"

req GET "$API/api/share/aaaaaaaaaaaaaaaaaaaaaa" ""
check "an unknown token is 404 not_found" \
  "$([ "$CODE" = 404 ] && [ "$(jget "$BODY" 'd["error"]["code"]')" = not_found ] && echo 0 || echo 1)" "got $CODE $BODY"

req PATCH "$API/api/admin/shares/$TOK" "$COOKIE_H" '{"isActive":false}'
check "the dashboard can revoke a link" "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE $BODY"
req GET "$API/api/share/$TOK" ""
check "a revoked token answers 404, indistinguishable from unknown" \
  "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE $BODY"

# An expired link is made the only way an operator can make one: an explicit past
# expiresAt on the dashboard. The create path refuses it (a non-positive duration means
# "never"), so this is not reachable through the public API by accident.
PAST_MS=$(( ($(date +%s) - 3600) * 1000 ))
req POST "$API/api/reports" "$AGENT_H" "{\"title\":\"$STAMP expired\",\"content\":\"x\"}"
ETOK=$(jget "$BODY" 'd["data"]["share"]["token"]')
req PATCH "$API/api/admin/shares/$ETOK" "$COOKIE_H" "{\"expiresAt\":$PAST_MS}"
check "the dashboard can set a past expiry" "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE $BODY"
req GET "$API/api/share/$ETOK" ""
check "an EXPIRED token answers 410 gone (and only expiry does)" \
  "$([ "$CODE" = 410 ] && [ "$(jget "$BODY" 'd["error"]["code"]')" = gone ] && echo 0 || echo 1)" "got $CODE $BODY"

echo
echo "==> 5. ownership boundary answers 404, never 403"
req POST "$API/api/admin/keys" "$COOKIE_H" "{\"name\":\"$STAMP other\",\"expiresInHours\":1}"
OTHER=$(jget "$BODY" 'd["data"]["token"]')
check "the dashboard can mint a second key" "$([ "$CODE" = 201 ] && [ -n "$OTHER" ] && echo 0 || echo 1)" "got $CODE $BODY"
OTHER_H="Authorization: Bearer $OTHER"

req GET "$API/api/reports/$RID" "$OTHER_H"
check "another key reading the first key's report is 404, NOT 403" \
  "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE $BODY"
req PATCH "$API/api/reports/$RID" "$OTHER_H" '{"title":"stolen"}'
check "another key PATCHing it is 404 too" "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE $BODY"
req DELETE "$API/api/reports/$RID" "$OTHER_H"
check "another key DELETEing it is 404 too" "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE $BODY"
req GET "$API/api/reports/$RID" "$AGENT_H"
check "the owner's report survived all three attempts" \
  "$([ "$CODE" = 200 ] && [ "$(jget "$BODY" 'd["data"]["title"]')" = "$STAMP patched" ] && echo 0 || echo 1)" "got $CODE $(jget "$BODY" 'd["data"]["title"]')"
req GET "$API/api/reports" "$OTHER_H"
check "the other key's listing does not leak the first key's report" \
  "$(grep_qF "$BODY" "$RID" && echo 1 || echo 0)" "listing=${BODY:0:120}"
req GET "$API/api/reports" "$AGENT_H"
check "root's own listing DOES contain it" \
  "$(grep_qF "$BODY" "$RID" && echo 0 || echo 1)"
req GET "$API/api/reports" ""
check "the listing without a credential is 401, not an empty list" \
  "$([ "$CODE" = 401 ] && echo 0 || echo 1)" "got $CODE"

echo
echo '==> 6. format:"html" is served verbatim, and only through its own endpoint'
HTMLBODY='<html><body><h1>verbatim</h1><script>window.__p4=1</script></body></html>'
HTML_JSON="$(python3 -c 'import json,sys; print(json.dumps({"title":sys.argv[2],"content":sys.argv[1],"format":"html","autoShareHours":0}))' "$HTMLBODY" "$STAMP html")"
req POST "$API/api/reports" "$AGENT_H" "$HTML_JSON"
HTOK=$(jget "$BODY" 'd["data"]["share"]["token"]')
check "a format:html report is created" "$([ "$CODE" = 201 ] && [ -n "$HTOK" ] && echo 0 || echo 1)" "got $CODE ${BODY:0:140}"
RAW=$(curl -sS "$API/s/$HTOK/html")
check "GET /s/{token}/html returns the bytes unchanged (no escaping, no nonce)" \
  "$([ "$RAW" = "$HTMLBODY" ] && echo 0 || echo 1)" "got ${RAW:0:90}"
check "the html body is framed by the share page, not the SPA" \
  "$(curl -sS "$API/s/$HTOK" | grep -q 'report-frame' && echo 0 || echo 1)"
req GET "$API/s/aaaaaaaaaaaaaaaaaaaaaa/html" ""
check "an unknown token's /html is 404" "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE"

echo
echo "==> 7. one-time credential: only in the fragment, HEAD refuses, claim is single-use"
req POST "$API/api/admin/key-handoffs" "$COOKIE_H" "{\"label\":\"$STAMP handoff\"}"
HID=$(jget "$BODY" 'd["data"]["id"]')
HSECRET=$(jget "$BODY" 'd["data"]["claim_secret"]')
HURL=$(jget "$BODY" 'd["data"]["url"]')
check "the hand-off link carries the secret in the fragment" \
  "$(grep_qF "$HURL" "k/$HID#" && echo 0 || echo 1)" "got $HURL"
check "the hand-off URL has no query string (a fragment never reaches the server)" \
  "$(grep_qF "$HURL" '?' && echo 1 || echo 0)" "got $HURL"

PAGE=$(curl -sS "$API/k/$HID")
check "the hand-off page's claim button is hidden without JavaScript" \
  "$(grep_qF "$PAGE" 'hidden' && echo 0 || echo 1)"

CLAIM_H="X-Teleport-Claim: $HSECRET"

# Order matters here and this is the whole point of the check: for an APPROVED
# application a successful GET *is* the claim — it mints the derived credential and
# flips the row to claimed. So the claim secret may be presented successfully exactly
# once, and the response of that one call is the only place the token ever exists.
# A negative check (wrong secret, missing header) must therefore come first, and the
# successful call must be the last thing done with this secret.
req GET "$API/api/agent-keys/applications/$HID" "X-Teleport-Claim: wrong-secret"
check "a wrong claim secret is 404 (no existence oracle)" "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE $BODY"
req GET "$API/api/agent-keys/applications/$HID" ""
check "no claim header at all is 404 too" "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE"

# HEAD is refused before any of that, which is what protects the link from a
# prefetcher. Probed here because it must also not consume anything.
head_probe "$API/api/agent-keys/applications/$HID" "$CLAIM_H"
check "HEAD on the claim endpoint is 405 method_not_allowed" \
  "$([ "$CODE" = 405 ] && echo 0 || echo 1)" "got $CODE"
check "the 405 advertises Allow: GET" "$([ "$ALLOW" = GET ] && echo 0 || echo 1)" "got '$ALLOW'"

# The one and only successful claim.
req GET "$API/api/agent-keys/applications/$HID" "$CLAIM_H"
CLAIMED=$(jget "$BODY" 'd["data"]["key"]["token"]')
check "the real secret claims the credential" \
  "$([ "$CODE" = 200 ] && [ -n "$CLAIMED" ] && echo 0 || echo 1)" "got $CODE ${BODY:0:160}"
check "the claim response carries the derived key's metadata" \
  "$([ "$(jget "$BODY" 'd["data"]["key"]["id"]')" != "" ] && [ "$(jget "$BODY" 'd["data"]["key"]["token_prefix"]')" != "" ] && echo 0 || echo 1)" "${BODY:0:160}"
check "the claimed key's name is the hand-off label" \
  "$([ "$(jget "$BODY" 'd["data"]["key"]["name"]')" = "$STAMP handoff" ] && echo 0 || echo 1)" "got $(jget "$BODY" 'd["data"]["key"]["name"]')"
req GET "$API/api/agent-keys/me" "Authorization: Bearer $CLAIMED"
check "the claimed token authenticates against the agent API" \
  "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE $BODY"
check "the key is reported with a prefix, never the full token" \
  "$(grep_qF "$BODY" "$CLAIMED" && echo 1 || echo 0)"

# Second attempt: the credential is single-use, and the refusal is folded into 404 so
# it cannot confirm that the id exists.
req GET "$API/api/agent-keys/applications/$HID" "$CLAIM_H"
check "the credential cannot be claimed twice" \
  "$([ "$CODE" = 404 ] && echo 0 || echo 1)" "got $CODE $BODY"

echo
echo "==> 8. renewal only extends, never shortens"
req GET "$API/api/agent-keys/me" "$OTHER_H"
BEFORE=$(jget "$BODY" 'd["data"]["expires_at"]')
req POST "$API/api/agent-keys/renewals" "$OTHER_H" '{"requestedHours":72}'
RENEWAL=$(jget "$BODY" 'd["data"]["id"]')
check "the key can file a renewal" "$([ "$CODE" = 201 ] && [ -n "$RENEWAL" ] && echo 0 || echo 1)" "got $CODE $BODY"
req POST "$API/api/admin/key-renewals/$RENEWAL/approve" "$COOKIE_H" '{"grantedHours":72}'
check "the dashboard can approve it" "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE $BODY"
req GET "$API/api/agent-keys/me" "$OTHER_H"
AFTER=$(jget "$BODY" 'd["data"]["expires_at"]')
check "the renewal EXTENDED the expiry past its old value" \
  "$([ -n "$BEFORE" ] && [ -n "$AFTER" ] && [ "$AFTER" -gt "$BEFORE" ] && echo 0 || echo 1)" "before=$BEFORE after=$AFTER"
# The rule the contract names: an early renewal keeps the time already paid for, so the
# new expiry is (old expiry + granted), not (now + granted).
EXPECTED=$(( BEFORE + 72 * 3600 * 1000 ))
check "an early renewal keeps the remaining time (old expiry + granted)" \
  "$([ "$AFTER" = "$EXPECTED" ] && echo 0 || echo 1)" "after=$AFTER expected=$EXPECTED"

echo
echo "==> 9. the AI guide and the SPA are both served by this process"
for path in /ai /ai.md /llms.txt; do
  req GET "$API$path" ""
  check "GET $path is 200 with no credentials" "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE"
done
GUIDE=$(curl -sS "$API/ai.md")
check "the guide's placeholders are all substituted (no '{{' left)" \
  "$(grep_qF "$GUIDE" '{{' && echo 1 || echo 0)" "left=$(grep -o -m3 '{{[A-Z_]*}}' "$GUIDE_FILE" 2>/dev/null | tr '\n' ' ')"
# The guide is ~39 KB. It goes to a file once and every read is a FILE read: see
# the grep_q helper for why piping a large body into `grep -q` is a pipefail trap.
GUIDE_FILE="$(mktemp)"; printf '%s' "$GUIDE" > "$GUIDE_FILE"
GUIDE_OK="$(grep_qF "$GUIDE" "$API" && echo 0 || echo 1)"
check "the guide states the live base URL (AppBaseURL = origin + prefix)" \
  "$GUIDE_OK" "len=${#GUIDE} want=$API"
if [ "$GUIDE_OK" != 0 ]; then
  # Keep the evidence: this check failed intermittently during the P6 cut-over and
  # the body was not retained, which left the cause undecidable. A check that
  # reports a failure must leave the input that produced it.
  mkdir -p bin/e2e 2>/dev/null || true
  cp "$GUIDE_FILE" bin/e2e/guide-failed.md 2>/dev/null || true
  echo "  (saved the failing body to bin/e2e/guide-failed.md)"
fi

SHELL_HTML=$(curl -sS "$API/")
SHELL_HTML_FILE="$(mktemp)"; printf '%s' "$SHELL_HTML" > "$SHELL_HTML_FILE"
check "the prefix root serves the SPA shell (not an API 404)" \
  "$(grep_qE "$SHELL_HTML" "$APP_ID_RE" && echo 0 || echo 1)" "${SHELL_HTML:0:80}"
# -m1 rather than `| head -1`: head closing the pipe early sends SIGPIPE upstream and
# pipefail then reports the substitution as failed, which would extract an EMPTY
# asset URL and make the next check test an empty string instead of the asset.
ASSET=$(grep -oE -m1 "$P/assets/[A-Za-z0-9._-]+\.js" "$SHELL_HTML_FILE" || true)
check "the shell references a hashed asset under the prefix" "$([ -n "$ASSET" ] && echo 0 || echo 1)" "got '$ASSET'"
if [ -n "$ASSET" ]; then
  req GET "$BASE$ASSET" ""
  check "the referenced asset is served" "$([ "$CODE" = 200 ] && echo 0 || echo 1)" "got $CODE for $ASSET"
fi

# The html report's share page is live, so its headers can be inspected. The CSP is a
# header, and it is the header that constrains the iframe the page embeds.
CSPHEAD=$(curl -sSI "$API/s/$HTOK" | tr -d '\r' | awk 'tolower($1)=="content-security-policy:"{ sub(/^[^:]*: /,""); print }')
check "the share page carries a deny-by-default CSP" \
  "$(grep_qF "$CSPHEAD" "default-src 'none'" && echo 0 || echo 1)" "got '$CSPHEAD'"
check "the CSP allows only same-origin frames and scripts" \
  "$(grep_qF "$CSPHEAD" "frame-src 'self'" && grep_qF "$CSPHEAD" "script-src 'self'" && echo 0 || echo 1)" "got '$CSPHEAD'"
# The share body must NOT contain the SPA's app element. Fetched to a file (not
# `curl | grep -q`) for the same SIGPIPE reason as everything above: in a
# "must not contain X" check, a killed grep reports failure exactly when X is
# present, which silently turns the check into a PASS.
SHARE_BODY_FILE="$(mktemp)"; curl -sS -o "$SHARE_BODY_FILE" "$API/s/$HTOK"
check "the share page body is server-rendered, not the SPA shell" \
  "$(grep_qE "$(cat "$SHARE_BODY_FILE")" "$APP_ID_RE" && echo 1 || echo 0)"

rm -f "$GUIDE_FILE" "$SHELL_HTML_FILE" "$SHARE_BODY_FILE" 2>/dev/null || true
echo
echo "----- P4 contract: $pass passed, $fail failed -----"
if [ "$fail" != 0 ]; then exit 1; fi
exit 0
