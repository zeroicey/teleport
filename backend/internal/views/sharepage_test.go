package views

import (
	"strings"
	"testing"

	"github.com/zeroicey/teleport/backend/internal/domain"
)

func shareFor(format domain.ReportFormat) SharePageOptions {
	return SharePageOptions{
		Report: domain.Report{
			ID:       "r1",
			Title:    "T",
			Category: "general",
			Format:   format,
			Content:  "<h1>仪表盘</h1><script>document.title='ran'</script>",
		},
		Share:         domain.ShareToken{Token: "tok"},
		AssetBaseURL:  "https://example.test/p",
		HTMLReportURL: "https://example.test/p/s/tok/html",
	}
}

// The one place raw report HTML is allowed to reach the browser is the report
// sub-resource — never the share page itself. This asserts the share page
// frames it instead of inlining it.
func TestHTMLReportIsFramedNotSpliced(t *testing.T) {
	page := RenderSharePage(shareFor(domain.FormatHTML))

	want := `<iframe class="report-frame" src="https://example.test/p/s/tok/html"`
	if !strings.Contains(page, want) {
		t.Fatalf("missing frame %q in:\n%s", want, page)
	}
	if strings.Contains(page, "<h1>仪表盘</h1>") {
		t.Error("report body was spliced into the share page")
	}
	if !strings.Contains(page, `<div class="wrap wrap-wide">`) {
		t.Error("html report should get the wider column")
	}
	// The frame element must be balanced; a truncated page would still contain
	// the opening tag, so assert the closing one too.
	if !strings.Contains(page, "</iframe>") {
		t.Error("iframe is not closed")
	}
	// Default = NOT sandboxed is a recorded decision (the report keeps
	// localStorage/cookies), not an oversight. Assert it against the untouched
	// global, so flipping that default cannot slip through: the knob test below
	// assigns the global explicitly and would not notice.
	if strings.Contains(page, "sandbox=") {
		t.Error("report frame is sandboxed by default; that contradicts " +
			".ai/decisions/2026-10-11-html-report-rendering.md (D2)")
	}
}

// A markdown report must be untouched by any of the above: no frame, no
// wrap-wide, and raw HTML still stripped.
func TestMarkdownReportIsNotFramed(t *testing.T) {
	opts := shareFor(domain.FormatMarkdown)
	opts.Report.Content = "# 标题\n\n<script>alert('x')</script>\n"
	page := RenderSharePage(opts)

	if strings.Contains(page, "<iframe") {
		t.Error("markdown report must not be framed")
	}
	// Match the element, not the string "wrap-wide": the inline <style> carries
	// a `.wrap-wide` rule on every page, so a bare substring check passes even
	// when the class is never applied (it did, until this test was tightened).
	if !strings.Contains(page, `<div class="wrap">`) {
		t.Error("markdown report must keep the 820px reading column")
	}
	if strings.Contains(page, `class="wrap wrap-wide"`) {
		t.Error("markdown report must not get the wider column")
	}
	if strings.Contains(page, "<script>alert('x')</script>") {
		t.Error("raw HTML from a markdown report reached the page")
	}
	if !strings.Contains(page, "raw HTML omitted") {
		t.Error("expected goldmark to replace the raw HTML block")
	}
}

// The "open on its own" link must point at the same sub-resource as the frame,
// otherwise the escape hatch from a cramped frame would 404.
func TestHTMLReportOffersTheSameURLStandalone(t *testing.T) {
	page := RenderSharePage(shareFor(domain.FormatHTML))
	if !strings.Contains(page, `href="https://example.test/p/s/tok/html"`) {
		t.Error("standalone link does not point at the report sub-resource")
	}
	if strings.Count(page, "https://example.test/p/s/tok/html") != 2 {
		t.Errorf("expected the sub-resource URL exactly twice (frame + link), got %d",
			strings.Count(page, "https://example.test/p/s/tok/html"))
	}
}

// HTMLReportSandbox documents an isolation escape hatch, so the branch that
// consumes it must actually work — otherwise flipping that one line would
// silently produce an unsandboxed frame that still looks "configured".
func TestReportFrameSandboxKnob(t *testing.T) {
	original := HTMLReportSandbox
	// Restore even if an assertion below calls t.Fatal: a leaked global would
	// silently change every other test in this package.
	t.Cleanup(func() { HTMLReportSandbox = original })

	HTMLReportSandbox = ""
	if page := RenderSharePage(shareFor(domain.FormatHTML)); strings.Contains(page, "sandbox=") {
		t.Error("empty HTMLReportSandbox must not emit a sandbox attribute")
	}

	HTMLReportSandbox = "allow-scripts allow-popups"
	page := RenderSharePage(shareFor(domain.FormatHTML))
	if !strings.Contains(page, `sandbox="allow-scripts allow-popups"`) {
		t.Errorf("sandbox attribute missing or escaped wrongly: %s", page)
	}
}

// The outer page's policy must stay strict; the frame is the only addition.
func TestOuterCSPKeepsScriptsStrict(t *testing.T) {
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"frame-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(ContentSecurityPolicy, want) {
			t.Errorf("ContentSecurityPolicy missing %q", want)
		}
	}
	if strings.Contains(ContentSecurityPolicy, "unsafe-inline' 'unsafe-eval'") {
		t.Error("outer CSP must not carry the report document's script relaxation")
	}

	// The page links the self-hosted webfont stylesheet, so its policy has to
	// permit both halves of that: `style-src` for the <link>, and `font-src` for
	// the @font-face fetches the stylesheet triggers. Either one missing is
	// silent — the page renders and quietly falls back to a system face. This
	// exact mistake shipped on the /ai page, which is why both are pinned here
	// as well as in the aidoc package.
	for _, want := range []string{"style-src 'self'", "font-src 'self'"} {
		if !strings.Contains(ContentSecurityPolicy, want) {
			t.Errorf("ContentSecurityPolicy missing %q but the page links the webfont stylesheet", want)
		}
	}

	// ...and the relaxation must not extend to scripts.
	if !strings.Contains(ContentSecurityPolicy, "script-src 'self'") ||
		strings.Contains(ContentSecurityPolicy, "script-src 'self' 'unsafe-inline'") {
		t.Error("font/style relaxation must not have widened script-src")
	}

	// The report document is the only place inline/remote script is allowed.
	if !strings.Contains(HTMLReportContentSecurityPolicy, "unsafe-inline") {
		t.Error("report document CSP should allow inline script")
	}
	if !strings.Contains(HTMLReportContentSecurityPolicy, "frame-ancestors 'self'") {
		t.Error("report document must only be framable by us")
	}
	if HTMLReportContentSecurityPolicy == ContentSecurityPolicy {
		t.Error("the two policies are the same string; one of them is wrong")
	}
}
