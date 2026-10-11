package aidoc

import (
	"strings"
	"testing"
)

// The HTML rendering is a separate delivery path from the Markdown one, so it
// gets its own contract checks.
func TestHTMLRendersGuide(t *testing.T) {
	f := testFacts()
	got := HTML(f)

	for _, want := range []struct{ what, needle string }{
		{"doctype", "<!DOCTYPE html>"},
		{"language", `lang="zh-CN"`},
		{"title includes the deployment", f.AppBase},
		{"noindex", `content="noindex, nofollow, noarchive"`},
		{"guide heading rendered as HTML", "<h1"},
		{"section heading rendered", "<h2"},
		{"tables rendered", "<table>"},
		{"code blocks rendered", "<pre"},
	} {
		if !strings.Contains(got, want.needle) {
			t.Errorf("HTML guide omits %s (%q)", want.what, want.needle)
		}
	}

	if strings.Contains(got, "{{") {
		t.Error("HTML guide contains an unresolved placeholder")
	}
}

// The page must not execute anything. It has no script tag, and the CSP has no
// script-src at all — so even an injected tag would be blocked.
func TestHTMLHasNoScript(t *testing.T) {
	got := HTML(testFacts())

	if strings.Contains(strings.ToLower(got), "<script") {
		t.Error("guide HTML must not contain a <script> tag")
	}
	if strings.Contains(got, "onclick") || strings.Contains(got, "onload") {
		t.Error("guide HTML must not contain inline event handlers")
	}
	if strings.Contains(CSP, "script-src") {
		t.Error("CSP should not grant script-src; giving it one weakens the page for no benefit")
	}
	if !strings.Contains(CSP, "default-src 'none'") {
		t.Error("CSP should deny by default")
	}
}

// A guide rendered as the literal string "{{APP_BASE}}/api/reports" is worse
// than useless, so assert the substitution reaches the HTML path too.
func TestHTMLSubstitutesPlaceholders(t *testing.T) {
	f := testFacts()
	f.AppBase = "https://other.test/x"
	got := HTML(f)

	if !strings.Contains(got, "https://other.test/x") {
		t.Error("HTML guide did not pick up the configured base URL")
	}
	if strings.Contains(got, "api.hcyj.xyz") {
		t.Error("HTML guide leaked the old host")
	}
}

// Markdown content must be escaped, not injected: the guide is Markdown we
// control today, but the renderer path is the same one used for untrusted
// reports, so a regression here would be a real XSS.
func TestHTMLDoesNotEmitRawScriptFromContent(t *testing.T) {
	got := HTML(testFacts())
	if strings.Contains(got, "<img src=x onerror") {
		t.Error("unexpected raw HTML survived rendering")
	}
}

func TestLLMSTxtPointsAtTheGuide(t *testing.T) {
	f := testFacts()
	got := LLMSTxt(f)

	for _, want := range []string{
		f.AppBase + "/ai.md",
		f.AppBase + "/ai",
		f.AppBase + "/api/health",
		"Bearer",
		"markdown",
		// The key is self-service. An agent that reads only llms.txt must not be
		// sent to the user, who cannot read an agent's key back out by design.
		f.AppBase + "/api/agent-keys/applications",
		"X-Teleport-Claim",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("llms.txt omits %q", want)
		}
	}
	if strings.Contains(got, "{{") {
		t.Error("llms.txt contains an unresolved placeholder")
	}
}

// Tripwire for a drift that shipped once: llms.txt kept telling agents to ask
// the user for a key long after keys became self-service. It is the discovery
// file, so that is exactly where the wrong instruction does the most damage —
// and nothing else caught it, because the canonical guide was already correct.
//
// The needles are deliberately broad: pinning only the exact sentence that
// shipped would let a reworded version ("ask the user for a key", "request it
// from the user") through.
func TestLLMSTxtDoesNotSendAgentsToTheUserForAKey(t *testing.T) {
	got := strings.ToLower(LLMSTxt(testFacts()))
	for _, forbidden := range []string{
		"ask the user",
		"from the user",
		"the user for a key",
		"user for the key",
		"not published here",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("llms.txt contains %q; keys are self-service, the user cannot hand one over", forbidden)
		}
	}
	// ...and it must positively route agents to the self-service endpoint, so
	// that removing the instruction entirely also fails rather than passing
	// vacuously.
	if !strings.Contains(got, "/api/agent-keys/applications") {
		t.Error("llms.txt does not point at the self-service application endpoint")
	}
}

// llms.txt must not become a second copy of the guide — the whole point is that
// there is one canonical document that cannot drift from itself.
func TestLLMSTxtStaysShort(t *testing.T) {
	got := LLMSTxt(testFacts())
	if n := strings.Count(got, "\n"); n > 30 {
		t.Errorf("llms.txt is %d lines; it should stay a pointer, not a copy", n)
	}
}

// It must never publish the key either.
func TestLLMSTxtHasNoSecret(t *testing.T) {
	got := LLMSTxt(testFacts())
	for _, forbidden := range []string{"AGENT_SECRET_KEY=", "BIOWGC"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("llms.txt contains secret material %q", forbidden)
		}
	}
}

// TestCSPAllowsExactlyWhatThePageLoads is the guard for a defect that reached
// production.
//
// The self-hosted webfonts were linked from this page while its CSP still said
// `style-src 'unsafe-inline'` with no `font-src`. `default-src 'none'` then
// blocked both the stylesheet and the font fetch. Nothing failed: the page
// rendered, the response was 200, and the text silently fell back to a system
// face. It shipped because the local harness served no CSP header at all, so
// only a real browser against the real server could see it.
//
// The rule this pins: every external resource the page references must be
// permitted by the policy the page is served with. Asserting the two sides
// against each other is what makes the next such addition fail loudly.
func TestCSPAllowsExactlyWhatThePageLoads(t *testing.T) {
	got := HTML(testFacts())

	// The page links a stylesheet and an icon; both are same-origin.
	refs := map[string]string{
		"stylesheet": `rel="stylesheet"`,
		"icon":       `rel="icon"`,
	}
	for what, marker := range refs {
		if !strings.Contains(got, marker) {
			t.Fatalf("page no longer declares a %s; update this test with the change", what)
		}
	}

	// A same-origin stylesheet needs style-src 'self'; the @font-face fetches it
	// triggers need font-src 'self'. Neither is implied by 'unsafe-inline'.
	if !strings.Contains(CSP, "style-src 'self'") {
		t.Error("CSP lacks style-src 'self' but the page links a stylesheet; the link will be blocked")
	}
	if !strings.Contains(CSP, "font-src 'self'") {
		t.Error("CSP lacks font-src 'self' but the linked stylesheet declares @font-face; the fonts will be blocked")
	}
	if !strings.Contains(CSP, "img-src 'self'") {
		t.Error("CSP lacks img-src 'self' but the page links an icon")
	}

	// ...and the relaxation must stay scoped. This page is script-free by
	// design; granting script-src would give up that property for nothing.
	if strings.Contains(CSP, "script-src") {
		t.Error("CSP grants script-src; this page has no scripts and must not permit any")
	}
	if !strings.Contains(CSP, "default-src 'none'") {
		t.Error("CSP must keep denying by default")
	}
}
