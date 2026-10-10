package aidoc

import (
	"strings"
	"testing"
	"time"

	"github.com/zeroicey/teleport/backend/internal/config"
	"github.com/zeroicey/teleport/backend/internal/views"
)

func testFacts() Facts {
	return Facts{
		AppBase:           "https://api.hcyj.xyz/yeciorez/teleport",
		RoutePrefix:       "/yeciorez/teleport",
		DefaultShareHours: 168,
		MaxContentBytes:   1048576,
		Environment:       "production",
	}
}

// The guide is injected into an agent's context and used as an API contract, so
// a leftover placeholder would be read as a literal URL to call. This is the
// test that makes "ship the guide" safe.
func TestNoUnresolvedPlaceholders(t *testing.T) {
	got := Markdown(testFacts())
	if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
		for i, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "{{") {
				t.Errorf("unresolved placeholder at line %d: %s", i+1, line)
			}
		}
	}
}

// Substitution must actually happen — a passing "no placeholder" check would
// also pass if the replacer silently did nothing to an empty template.
func TestPlaceholdersAreSubstituted(t *testing.T) {
	f := testFacts()
	got := Markdown(f)

	for _, want := range []string{f.AppBase, f.RoutePrefix, "168", "1048576", f.Environment} {
		if !strings.Contains(got, want) {
			t.Errorf("guide does not contain expected substituted value %q", want)
		}
	}
}

// Each placeholder constant must appear in the template, or it is dead code
// that gives a false sense of what the guide can adapt to.
func TestEveryPlaceholderIsUsed(t *testing.T) {
	for _, ph := range []string{phAppBase, phRoutePrefix, phDefaultShareHrs, phMaxContent, phEnvironment} {
		if !strings.Contains(guideTemplate, ph) {
			t.Errorf("placeholder %s is declared but never used in guide.md", ph)
		}
	}
}

// Changing the deployment must change the guide. This is the anti-drift property
// that motivated server-side substitution in the first place.
func TestGuideFollowsConfiguration(t *testing.T) {
	f := testFacts()
	f.AppBase = "https://example.test/prefix"
	f.RoutePrefix = "/prefix"
	f.DefaultShareHours = 24
	f.MaxContentBytes = 2048

	got := Markdown(f)
	if !strings.Contains(got, "https://example.test/prefix") {
		t.Error("guide does not track AppBase")
	}
	if strings.Contains(got, "https://api.hcyj.xyz") {
		t.Error("guide still contains the old host after reconfiguration")
	}
	if !strings.Contains(got, "24 小时") {
		t.Error("guide does not track DefaultShareHours")
	}
	if strings.Contains(got, "1048576") {
		t.Error("guide still contains the old content limit")
	}
}

// The key facts an agent acts on. If someone rewrites the guide and drops one of
// these, the guide stops being a usable contract — so assert them directly
// rather than trusting prose review.
func TestGuideDocumentsTheContract(t *testing.T) {
	got := Markdown(testFacts())

	for _, want := range []struct{ what, needle string }{
		{"create-report endpoint", "/api/reports"},
		{"revoke endpoint", "/revoke"},
		{"health endpoint", "/api/health"},
		{"bearer scheme", "Authorization: Bearer"},
		{"the only required credential", "AGENT_SECRET_KEY"},
		{"share url field to hand back", "data.share.url"},
		{"expired-link status", "410"},
		{"revoked/unknown-link status", "404"},
		{"unauthorized status", "401"},
		{"markdown is the working format", `"markdown"`},
		{"html format caveat", "HTML 渲染尚未启用"},
		{"epoch-milliseconds convention", "UNIX epoch 毫秒"},
		{"skill-authoring section", "变成你自己的 skill"},
	} {
		if !strings.Contains(got, want.needle) {
			t.Errorf("guide omits %s (expected to mention %q)", want.what, want.needle)
		}
	}
}

// The guide is written for agents but must not itself leak the secret, and must
// not hand out a copy-pasteable placeholder that looks like a real key.
func TestGuideContainsNoSecret(t *testing.T) {
	got := Markdown(testFacts())

	for _, forbidden := range []string{
		"AGENT_SECRET_KEY=",   // an assignment, i.e. a value follows
		"SESSION_SECRET",      // the guide has no business naming this at all
		"ADMIN_PASSWORD_HASH", // nor this
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("guide mentions %q — the guide must not carry secret material", forbidden)
		}
	}

	// It may and should name the *variable*, just never assign it a value.
	if !strings.Contains(got, "AGENT_SECRET_KEY") {
		t.Error("guide should tell the agent which credential to ask for")
	}
}

// The guide tells agents to use markdown because HTML is not rendered. If that
// ever changes, the caveat must change with it — this test is the tripwire.
func TestHTMLCaveatMatchesImplementation(t *testing.T) {
	got := Markdown(testFacts())
	if strings.Contains(got, "HTML 渲染尚未启用") && views.HTMLMountEnabled {
		t.Error("guide still says HTML rendering is disabled, but HTMLMountEnabled is true")
	}
}

// FactsFrom is the production wiring; make sure it reads the right fields rather
// than being a stub that happens to compile.
func TestFactsFromUsesAppBaseNotOrigin(t *testing.T) {
	cfg := &config.Config{
		RoutePrefix:       "/yeciorez/teleport",
		PublicBaseURL:     "https://api.hcyj.xyz",
		DefaultShareHours: 168,
		MaxContentBytes:   1048576,
		Environment:       "production",
	}
	f := FactsFrom(cfg)

	if f.AppBase != "https://api.hcyj.xyz/yeciorez/teleport" {
		t.Errorf("AppBase = %q, want origin+prefix", f.AppBase)
	}
	if f.RoutePrefix != cfg.RoutePrefix {
		t.Errorf("RoutePrefix = %q", f.RoutePrefix)
	}
	if f.DefaultShareHours != 168 || f.MaxContentBytes != 1048576 {
		t.Error("numeric facts not carried over")
	}
}

// Mirrors the guide being servable as a plain-text response.
func TestMarkdownIsPlainText(t *testing.T) {
	got := Markdown(testFacts())
	if !strings.HasPrefix(got, "# Teleport") {
		t.Errorf("guide should start with a level-1 heading, got %q", firstLine(got))
	}
	if strings.Contains(got, "\r\n") {
		t.Error("guide should use LF line endings")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// The guide embeds configured durations, so their rendering is part of the
// contract a reader sees. time.Duration.String() would give "24h0m0s", which
// reads like a machine value leaked into prose.
func TestHumanDurationIsReadable(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{45 * time.Second, "45s"},
		{90 * time.Minute, "1h30m"},
		{24 * time.Hour, "1d"},
		{7 * 24 * time.Hour, "7d"},
		{36 * time.Hour, "1d12h"},
		{0, "0s"},
		{-time.Hour, "0s"},
	}
	for _, c := range cases {
		if got := HumanDuration(c.in); got != c.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A rendered guide must never show a raw placeholder to a reader, and the key
// limits must actually reflect the configuration rather than the defaults.
func TestKeyLimitPlaceholdersFollowConfiguration(t *testing.T) {
	f := Facts{
		AppBase: "https://example.test/p", RoutePrefix: "/p", Environment: "test",
		KeyApplicationTTL: 48 * time.Hour,
		KeyClaimWindow:    15 * time.Minute,
		KeyApplyPerHour:   7,
	}
	got := Markdown(f)
	if strings.Contains(got, "{{") {
		t.Error("rendered guide still contains a placeholder")
	}
	for _, want := range []string{"2d", "15m", "7"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered guide is missing configured value %q", want)
		}
	}
}

// TestGuideKeepsTheRenewalGraceContract pins the two facts an agent cannot
// afford to get wrong about a lapsed key.
//
// Both were true statements in the guide until the 90-day grace window landed,
// and both then became actively harmful: an agent that still believed /me works
// while expired would conclude it was dead, re-apply, and silently lose access
// to every report it had ever published (a new key id does not inherit the old
// one's reports). The guide is the only place an agent learns this, so the
// statements are asserted rather than trusted.
func TestGuideKeepsTheRenewalGraceContract(t *testing.T) {
	got := Markdown(testFacts())

	// 1. The grace window must be described at all, with its length.
	for _, needle := range []string{"90 天", "宽限期", "KEY_RENEWAL_GRACE"} {
		if !strings.Contains(got, needle) {
			t.Errorf("guide never mentions %q; an agent cannot know a lapsed key is still recoverable", needle)
		}
	}

	// 2. The renewal endpoint must be stated as the surviving capability, and
	//    stated to keep the same key id — that is the reason to renew rather
	//    than re-apply.
	for _, needle := range []string{"/api/agent-keys/renewals", "key.id", "归属"} {
		if !strings.Contains(got, needle) {
			t.Errorf("guide omits %q from the grace-window rules", needle)
		}
	}

	// 3. The expiry headers must be documented, because they are the mechanism
	//    that makes "renew early" possible without polling /me.
	for _, needle := range []string{"X-Teleport-Key-Expires-At", "X-Teleport-Key-Expired"} {
		if !strings.Contains(got, needle) {
			t.Errorf("guide does not document %s", needle)
		}
	}

	// 4. The stale claim must be gone. Before the grace window, "once expired you
	//    can no longer submit a renewal" was correct; it is now false, and a
	//    guide that still says it tells agents to give up one paragraph after
	//    telling them not to.
	if strings.Contains(got, "无法再新提交") {
		t.Error("guide still says an expired key cannot submit a renewal; the grace window made that false")
	}
}

// TestGuideDocumentsTheOwnReportList covers the endpoint that makes ownership
// usable. Without it an agent can only read a report whose id it still
// remembers, and a mislaid id is unrecoverable by design (asking about someone
// else's is a 404), so the guide has to name it.
func TestGuideDocumentsTheOwnReportList(t *testing.T) {
	got := Markdown(testFacts())
	if !strings.Contains(got, "GET /api/reports`") && !strings.Contains(got, "`GET` | `{P}/api/reports`") {
		t.Error("guide does not document GET /api/reports (list your own)")
	}
	for _, needle := range []string{"owner_key_id", "只返回**你发布的**"} {
		if !strings.Contains(got, needle) {
			t.Errorf("guide's own-report list section omits %q", needle)
		}
	}
}
