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

// TestDashboardCSPAvoidsReportOnlyIncompatibleDirectives is the guard for a
// defect that a real browser found and no unit test would have.
//
// `upgrade-insecure-requests` is only valid in an ENFORCING policy. In a
// report-only policy Chrome ignores it and reports:
//
//	The Content Security Policy directive 'upgrade-insecure-requests' is ignored
//	when delivered in a report-only policy.
//
// That is a console error, not a policy violation. It therefore does not show up
// as a "violation" in any report — it shows up in scripts/verify-p4.sh's browser
// phase, which asserts a clean console. Since the policy ships report-only
// precisely so that a clean console can be observed before enforcing, a directive
// that guarantees a console error defeats the entire exercise.
//
// The directive is also pointless here: every source the dashboard policy allows
// is `'self'` or `data:`, so there is no plaintext subresource to upgrade.
func TestDashboardCSPAvoidsReportOnlyIncompatibleDirectives(t *testing.T) {
	// The set of directives Chrome ignores in report-only policies. Only
	// upgrade-insecure-requests is in common use; the others are listed so a
	// future edit is caught rather than argued about.
	incompatible := []string{
		"upgrade-insecure-requests",
		"block-all-mixed-content",
		"sandbox",
	}
	for _, directive := range incompatible {
		if strings.Contains(DashboardContentSecurityPolicy, directive) {
			t.Errorf("dashboard CSP contains %q, which is ignored (and reported as a console "+
				"error) in a report-only policy — the policy ships report-only first, so this "+
				"guarantees the console is never clean", directive)
		}
	}

	// The share page is served with an ENFORCING policy, where the directive is
	// valid — so its absence here must not have been "fixed" by copying that.
	if !strings.Contains(ContentSecurityPolicy, "upgrade-insecure-requests") {
		t.Log("note: the share page's enforcing policy no longer upgrades insecure requests")
	}
}

// TestDashboardSecurityHeadersSelectsTheHeaderName pins the polarity of the
// report-only/enforce switch, which is the switch that decides whether a policy
// mistake is a console message or a broken panel.
func TestDashboardSecurityHeadersSelectsTheHeaderName(t *testing.T) {
	reportOnly := DashboardSecurityHeaders(false)
	if _, ok := reportOnly["Content-Security-Policy"]; ok {
		t.Error("DashboardSecurityHeaders(false) set the ENFORCING header; false must mean report-only")
	}
	if got := reportOnly["Content-Security-Policy-Report-Only"]; got != DashboardContentSecurityPolicy {
		t.Errorf("report-only header = %q, want the dashboard policy", got)
	}

	enforcing := DashboardSecurityHeaders(true)
	if got := enforcing["Content-Security-Policy"]; got != DashboardContentSecurityPolicy {
		t.Errorf("enforcing header = %q, want the dashboard policy", got)
	}
	if _, ok := enforcing["Content-Security-Policy-Report-Only"]; ok {
		t.Error("DashboardSecurityHeaders(true) also set the report-only header; exactly one must go out")
	}

	// The non-CSP headers are present in both states.
	for _, headers := range []map[string]string{reportOnly, enforcing} {
		if headers["Permissions-Policy"] == "" {
			t.Error("Permissions-Policy missing")
		}
		if headers["X-Permitted-Cross-Domain-Policies"] != "none" {
			t.Error("X-Permitted-Cross-Domain-Policies missing")
		}
	}
}
