package views

import (
	"strings"
	"testing"
)

func handoffFixture(state HandoffState) KeyHandoffOptions {
	return KeyHandoffOptions{
		ID:            "8f14e45f-ceea-467a-9c1e-1b2c3d4e5f60",
		Label:         "A姐",
		State:         state,
		ClaimDeadline: 1_800_000_000_000,
		LinkExpiresAt: 1_800_000_000_000,
		AppBaseURL:    "https://api.example.com/prefix",
		AssetBaseURL:  "https://api.example.com/prefix",
		GuideURL:      "https://api.example.com/prefix/ai.md",
		KeyLifetime:   "24 小时",
	}
}

// TestHandoffPageEscapesTheLabel matters because the label is the only
// administrator-supplied value on the page: it is shown to a *recipient*, so an
// injection here would run in their browser rather than the admin's.
func TestHandoffPageEscapesTheLabel(t *testing.T) {
	opts := handoffFixture(HandoffOpen)
	opts.Label = `A姐"><script>alert(1)</script>`

	page := RenderKeyHandoffPage(opts)

	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("the label was spliced into the page as markup")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("the label was not escaped at all")
	}
	// The page's own bundle is the only script it may reference.
	if strings.Count(page, "<script") != 1 {
		t.Errorf("page has %d script tags, want exactly 1 (the module bundle)", strings.Count(page, "<script"))
	}
}

// TestHandoffPageIsInertWithoutJavaScript pins the anti-preview-bot property:
// a chat app's link preview fetches this page with no JS and no fragment, so it
// must not be able to trigger a claim. The button therefore ships hidden and is
// only revealed by the client script, which needs the fragment.
func TestHandoffPageIsInertWithoutJavaScript(t *testing.T) {
	page := RenderKeyHandoffPage(handoffFixture(HandoffOpen))

	if !strings.Contains(page, `id="handoff-claim" type="button" hidden`) {
		t.Error("the claim button is not hidden in the static HTML")
	}
	if !strings.Contains(page, `data-handoff-spent="0"`) {
		t.Error("an open link is not marked as claimable")
	}
	// The secret is never in the document: the server does not have it.
	if strings.Contains(page, "claim_secret") {
		t.Error("page mentions claim_secret; the secret must live only in the fragment")
	}
}

// TestHandoffPageStatesAreDistinguishable keeps the four outcomes from
// collapsing into one page, since each needs different instructions.
func TestHandoffPageStatesAreDistinguishable(t *testing.T) {
	cases := []struct {
		state HandoffState
		want  string
		spent string
	}{
		{HandoffOpen, "领取密钥", "0"},
		{HandoffClaimed, "已经被领取过了", "1"},
		{HandoffExpired, "过期", "1"},
		{HandoffUnknown, "无效", "1"},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			page := RenderKeyHandoffPage(handoffFixture(tc.state))
			if !strings.Contains(page, tc.want) {
				t.Errorf("page for state %q does not explain itself (want %q)", tc.state, tc.want)
			}
			if !strings.Contains(page, `data-handoff-spent="`+tc.spent+`"`) {
				t.Errorf("state %q: spent flag is not %q", tc.state, tc.spent)
			}
			if tc.spent == "1" && strings.Contains(page, `id="handoff-claim"`) {
				t.Errorf("state %q still renders a claim button", tc.state)
			}
		})
	}
}

// TestHandoffPagePointsAtTheGuide checks the one link that makes the page
// self-service: without it the recipient has a key and no idea what to do.
func TestHandoffPagePointsAtTheGuide(t *testing.T) {
	page := RenderKeyHandoffPage(handoffFixture(HandoffOpen))
	if !strings.Contains(page, "https://api.example.com/prefix/ai.md") {
		t.Error("page does not link the usage guide")
	}
	if !strings.Contains(page, "https://api.example.com/prefix/api/reports") {
		t.Error("page does not show the publish endpoint")
	}
	if !strings.Contains(page, "24 小时") {
		t.Error("page does not state the key lifetime")
	}
}
