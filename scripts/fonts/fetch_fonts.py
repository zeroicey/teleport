#!/usr/bin/env python3
"""Fetch and vendor the self-hosted webfonts into `web/public/fonts/`.

WHY THIS EXISTS
---------------
The dashboard and the public share pages must render in a real typeface, not
whatever the operating system happens to ship. Loading a font from a CDN is not
an option: this service is deliberately reachable from mainland China, where
`fonts.googleapis.com` / `fonts.gstatic.com` are unreliable at best, and the
share page's CSP is `font-src 'self'` regardless. So the font files are vendored
into the repository and compiled into the Go binary.

WHY A SCRIPT AND NOT A MANUAL DOWNLOAD
--------------------------------------
A pile of .woff2 files with no provenance is unreproducible: nobody can tell
which version is in the tree, or refresh it. This script records the exact
Google Fonts query for each family and regenerates the directory from scratch,
so the vendored files are a build artefact of a readable input.

WHAT IT PRODUCES
----------------
  web/public/fonts/fonts.css   all @font-face rules, the single source of truth
  web/public/fonts/*.woff2     the font binaries

Two properties are load-bearing and are asserted at the end of the run:

  1. Every `url(...)` in the emitted CSS is RELATIVE (`./name.woff2`). The CSS
     file is served from inside the route prefix (e.g.
     `/yeciorez/teleport/fonts/fonts.css`), so a relative reference resolves
     under the prefix automatically. An absolute `/fonts/...` would escape the
     prefix and 404 in production. Relative also means this same file works
     under `pnpm run dev:web` with no special-casing.

  2. `unicode-range` is preserved verbatim. Noto Sans SC is delivered as ~100
     slices, and that is the entire reason a Chinese page does not download
     4.5 MB of glyphs: the browser fetches only the slices whose unicode ranges
     the page actually uses. Dropping or rewriting those ranges would silently
     turn every page load into a multi-megabyte download.

WHY THE FONT FILENAMES CARRY A CONTENT HASH
-------------------------------------------
`slug-NNN-<sha256[:8]>.woff2`. The Go static handler only grants a year-long
`immutable` cache to URLs whose *name* proves the bytes can never change, and
everything else revalidates (`Cache-Control: no-cache`). Embedded files report a
zero modtime, so a revalidation cannot produce a 304 — a stable font URL would
mean the browser re-downloading hundreds of KB on every full page load. Hashing
the content into the name is what makes the long cache safe and correct; the
same reasoning as Vite's own `[name]-[hash].js` output. `fonts.css` keeps a
stable name on purpose (the Go templates must be able to link it without reading
a manifest) and revalidates — one request, not a hundred.

USAGE
-----
    python3 scripts/fonts/fetch_fonts.py

Needs network. Run it by hand when adding or updating a family; it is
deliberately NOT part of `pnpm run build`, because the release build must work
with no network at all.
"""

from __future__ import annotations

import hashlib
import re
import shutil
import sys
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
OUT_DIR = REPO / "web" / "public" / "fonts"

# A modern desktop Chrome UA. Google Fonts serves woff2 only to clients that
# advertise support; without this the CSS comes back with ttf urls, which are
# several times larger and universally supported by nothing we care about.
UA = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

# (family query for the CSS2 API, slug used for filenames)
#
# Outfit        — Latin UI and headings. A geometric sans with real character;
#                 deliberately not Inter/Roboto/system-ui, which the design
#                 guidance in use rejects as the generic default.
# Noto Sans SC  — CJK. Must cover ARBITRARY Chinese text, because the public
#                 share page renders AI-written Markdown reports whose content
#                 is not knowable at build time. Subsetting to the UI's own
#                 strings would break real reports, so the full unicode-range
#                 slice set is vendored instead.
# JetBrains Mono— report ids, tokens, code. Chosen for a tall x-height and
#                 unambiguous 0/O and 1/l/I, which matters when the value on
#                 screen is a credential being copied by hand.
FAMILIES = [
    ("Outfit:wght@400..700", "outfit"),
    ("Noto+Sans+SC:wght@400..700", "noto-sans-sc"),
    ("JetBrains+Mono:wght@400..700", "jetbrains-mono"),
]

CSS_URL = "https://fonts.googleapis.com/css2?family={query}&display=swap"
URL_RE = re.compile(r"url\((https://fonts\.gstatic\.com/[^)]+)\)")


def fetch(url: str) -> bytes:
    request = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(request, timeout=60) as response:
        return response.read()


def main() -> int:
    if OUT_DIR.exists():
        shutil.rmtree(OUT_DIR)
    OUT_DIR.mkdir(parents=True)

    blocks: list[str] = []
    total = 0

    for query, slug in FAMILIES:
        css_url = CSS_URL.format(query=query)
        print(f"==> {slug}: {css_url}")
        css = fetch(css_url).decode("utf-8")

        urls = URL_RE.findall(css)
        if not urls:
            print(f"    ERROR: no woff2 urls in the CSS for {slug}", file=sys.stderr)
            return 1

        # Number the slices in the order Google lists them and append a content
        # hash. Google's own filenames are opaque hashes; the index keeps the
        # directory readable, and the digest is what licenses the long cache
        # (see the module docstring).
        seen: dict[str, str] = {}
        for index, url in enumerate(urls):
            if url in seen:
                continue
            body = fetch(url)
            digest = hashlib.sha256(body).hexdigest()[:8]
            name = f"{slug}-{index:03d}-{digest}.woff2"
            (OUT_DIR / name).write_bytes(body)
            seen[url] = name
            total += len(body)
            print(f"    {name}  {len(body):>8,} bytes")

        # Rewrite every absolute gstatic url to a relative reference. Only the
        # url() tokens are touched — font-family, font-weight, font-display and
        # unicode-range are preserved byte-for-byte.
        css = URL_RE.sub(lambda m: f"url(./{seen[m.group(1)]})", css)
        blocks.append(f"/* ---- {slug} ---- */\n{css.strip()}\n")

    header = (
        "/* GENERATED by scripts/fonts/fetch_fonts.py — do not edit by hand.\n"
        " *\n"
        " * Self-hosted webfonts, vendored so the single Go binary serves them\n"
        " * from its own origin under the route prefix. Referenced by\n"
        " * web/index.html (SPA) and by the Go share/handoff templates.\n"
        " *\n"
        " * All url() references are relative on purpose: this file is served\n"
        " * from inside the prefix, so relative paths follow the prefix without\n"
        " * any rewriting. See .ai/decisions/2026-10-12-self-hosted-webfonts.md.\n"
        " */\n\n"
    )
    (OUT_DIR / "fonts.css").write_text(header + "\n".join(blocks), encoding="utf-8")

    # --- assertions: the properties that must never silently regress ---------
    emitted = (OUT_DIR / "fonts.css").read_text(encoding="utf-8")
    absolute = re.findall(r"url\((/(?!/)[^)]+)\)", emitted)
    if absolute:
        print(f"    ERROR: absolute url() survived: {absolute[:3]}", file=sys.stderr)
        return 1
    if "unicode-range" not in emitted:
        print("    ERROR: unicode-range missing — CJK slicing was lost", file=sys.stderr)
        return 1

    # Every referenced binary must (a) exist and (b) carry a content hash, since
    # that hash is the whole basis for it being cached immutably. A rename back
    # to a stable filename would silently downgrade 100+ files to revalidate.
    referenced = re.findall(r"url\(\./([^)]+)\)", emitted)
    unhashed = [n for n in referenced if not re.fullmatch(r"[a-z0-9]+-\d{3}-[0-9a-f]{8}\.woff2", n)]
    if unhashed:
        print(f"    ERROR: unhashed font filename(s): {unhashed[:3]}", file=sys.stderr)
        return 1
    missing = [n for n in referenced if not (OUT_DIR / n).is_file()]
    if missing:
        print(f"    ERROR: fonts.css references missing file(s): {missing[:3]}", file=sys.stderr)
        return 1

    files = sorted(OUT_DIR.glob("*.woff2"))
    print()
    print(f"wrote {len(files)} woff2 files + fonts.css")
    print(f"total woff2 bytes: {total:,} ({total / 1024 / 1024:.2f} MiB)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
