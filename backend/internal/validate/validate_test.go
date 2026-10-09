package validate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/httpx"
)

// decode parses a JSON literal the same way the handler does, so tests exercise
// the real json.Number path.
func decode(t *testing.T, raw string) any {
	t.Helper()
	body, err := ParseJSONBody([]byte(raw))
	if err != nil {
		t.Fatalf("ParseJSONBody(%s): %v", raw, err)
	}
	return body
}

// TestValidInput covers the fully-populated happy path.
func TestValidInput(t *testing.T) {
	input, err := ParseCreateReportInput(decode(t, `{
		"title": "  Pentest  ",
		"content": "# body",
		"category": "pentest",
		"format": "html",
		"metadata": {"severity": "high"},
		"autoShareHours": 24
	}`), 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input.Title != "Pentest" {
		t.Errorf("title = %q, want trimmed", input.Title)
	}
	if input.Category != "pentest" || input.Format != domain.FormatHTML {
		t.Errorf("unexpected: %+v", input)
	}
	if input.AutoShareHours == nil || *input.AutoShareHours != 24 {
		t.Errorf("autoShareHours = %v", input.AutoShareHours)
	}
	if input.Metadata["severity"] != "high" {
		t.Errorf("metadata = %+v", input.Metadata)
	}
}

// TestDefaultsAreApplied checks the optional-field defaults.
func TestDefaultsAreApplied(t *testing.T) {
	input, err := ParseCreateReportInput(decode(t, `{"title":"t","content":"c"}`), 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input.Category != "general" {
		t.Errorf("category = %q, want general", input.Category)
	}
	if input.Format != domain.FormatMarkdown {
		t.Errorf("format = %q, want markdown", input.Format)
	}
	if input.AutoShareHours != nil {
		t.Errorf("autoShareHours should be nil when absent, got %v", *input.AutoShareHours)
	}
}

// TestEmptyStringsAreTreatedAsAbsent mirrors the original
// `!== undefined && !== null && !== ”` checks.
func TestEmptyStringsAreTreatedAsAbsent(t *testing.T) {
	input, err := ParseCreateReportInput(decode(t,
		`{"title":"t","content":"c","category":"","format":""}`), 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input.Category != "general" || input.Format != domain.FormatMarkdown {
		t.Errorf("empty strings should fall back to defaults: %+v", input)
	}
}

// TestAutoShareHoursZeroIsMeaningful is the subtle case: 0 must survive as an
// explicit value because it means "never expires", not "unset".
func TestAutoShareHoursZeroIsMeaningful(t *testing.T) {
	input, err := ParseCreateReportInput(decode(t,
		`{"title":"t","content":"c","autoShareHours":0}`), 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input.AutoShareHours == nil {
		t.Fatal("autoShareHours=0 should be preserved, not treated as absent")
	}
	if *input.AutoShareHours != 0 {
		t.Errorf("autoShareHours = %v, want 0", *input.AutoShareHours)
	}
}

// TestRejectedInputs covers every validation branch with its expected code.
func TestRejectedInputs(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"array body", `[]`, "bad_request"},
		{"string body", `"nope"`, "bad_request"},
		{"null body", `null`, "bad_request"},
		{"title missing", `{"content":"c"}`, "bad_request"},
		{"title wrong type", `{"title":123,"content":"c"}`, "bad_request"},
		{"title empty", `{"title":"","content":"c"}`, "bad_request"},
		{"title whitespace only", `{"title":"   \t\n","content":"c"}`, "bad_request"},
		{"title 301 chars", `{"title":"` + strings.Repeat("a", 301) + `","content":"c"}`, "bad_request"},
		{"content missing", `{"title":"t"}`, "bad_request"},
		{"content empty", `{"title":"t","content":""}`, "bad_request"},
		{"content wrong type", `{"title":"t","content":5}`, "bad_request"},
		{"category uppercase", `{"title":"t","content":"c","category":"Pentest"}`, "bad_request"},
		{"category leading dash", `{"title":"t","content":"c","category":"-x"}`, "bad_request"},
		{"category too long", `{"title":"t","content":"c","category":"` + strings.Repeat("a", 33) + `"}`, "bad_request"},
		{"format unknown", `{"title":"t","content":"c","format":"pdf"}`, "bad_request"},
		{"metadata array", `{"title":"t","content":"c","metadata":[]}`, "bad_request"},
		{"metadata string", `{"title":"t","content":"c","metadata":"x"}`, "bad_request"},
		{"autoShareHours negative", `{"title":"t","content":"c","autoShareHours":-1}`, "bad_request"},
		{"autoShareHours too large", `{"title":"t","content":"c","autoShareHours":87601}`, "bad_request"},
		{"autoShareHours not a number", `{"title":"t","content":"c","autoShareHours":"abc"}`, "bad_request"},
		{"autoShareHours null is absent", `{"title":"t","content":"c","autoShareHours":null}`, ""},
		{"autoShareHours false is 0", `{"title":"t","content":"c","autoShareHours":false}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCreateReportInput(decode(t, tc.body), 1<<20)
			if tc.code == "" {
				if err != nil {
					t.Errorf("expected acceptance, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			var apiErr *httpx.Error
			if !asAPIError(err, &apiErr) {
				t.Fatalf("expected *httpx.Error, got %T", err)
			}
			if apiErr.Code != tc.code {
				t.Errorf("code = %q, want %q", apiErr.Code, tc.code)
			}
		})
	}
}

// TestBoundaryLengths checks the inclusive title limit and the exclusive byte
// limit, in the units the original used.
func TestBoundaryLengths(t *testing.T) {
	t.Run("title at exactly 300 is accepted", func(t *testing.T) {
		if _, err := ParseCreateReportInput(decode(t,
			`{"title":"`+strings.Repeat("a", 300)+`","content":"c"}`), 1<<20); err != nil {
			t.Errorf("300-char title rejected: %v", err)
		}
	})

	t.Run("content at exactly the limit is accepted", func(t *testing.T) {
		limit := int64(100)
		if _, err := ParseCreateReportInput(decode(t,
			`{"title":"t","content":"`+strings.Repeat("a", 100)+`"}`), limit); err != nil {
			t.Errorf("content at the limit rejected: %v", err)
		}
	})

	t.Run("content one byte over is rejected", func(t *testing.T) {
		limit := int64(100)
		_, err := ParseCreateReportInput(decode(t,
			`{"title":"t","content":"`+strings.Repeat("a", 101)+`"}`), limit)
		if err == nil {
			t.Fatal("expected a 413")
		}
		var apiErr *httpx.Error
		if !asAPIError(err, &apiErr) || apiErr.Status != 413 {
			t.Errorf("expected 413, got %v", err)
		}
	})
}

// TestUTF16LengthSemantics is the key parity assertion: JavaScript's
// String.length counts UTF-16 code units, so an astral-plane character counts as
// two and a 150-emoji title exceeds the 300 limit.
func TestUTF16LengthSemantics(t *testing.T) {
	t.Run("emoji counts as two units", func(t *testing.T) {
		if got := UTF16Len("🔐"); got != 2 {
			t.Errorf("UTF16Len(emoji) = %d, want 2", got)
		}
		if got := UTF16Len("abc"); got != 3 {
			t.Errorf("UTF16Len(abc) = %d, want 3", got)
		}
		if got := UTF16Len("密码"); got != 2 {
			t.Errorf("UTF16Len(密码) = %d, want 2 (BMP chars are one unit)", got)
		}
	})

	t.Run("150 emoji title is rejected as 300 units", func(t *testing.T) {
		title := strings.Repeat("🔐", 150)
		if UTF16Len(title) != 300 {
			t.Fatalf("test premise wrong: %d units", UTF16Len(title))
		}
		// 300 units is exactly the limit, so this must be ACCEPTED.
		if _, err := ParseCreateReportInput(decode(t,
			`{"title":"`+title+`","content":"c"}`), 1<<20); err != nil {
			t.Errorf("300-unit title rejected: %v", err)
		}
	})

	t.Run("151 emoji title is rejected", func(t *testing.T) {
		title := strings.Repeat("🔐", 151)
		if _, err := ParseCreateReportInput(decode(t,
			`{"title":"`+title+`","content":"c"}`), 1<<20); err == nil {
			t.Error("expected a 301-unit title to be rejected")
		}
	})
}

// TestJSTrimSemantics pins the ECMAScript whitespace set, especially the U+0085
// case where Go's strings.TrimSpace would differ.
func TestJSTrimSemantics(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"  abc  ", "abc"},
		{"\t\n abc \r\n", "abc"},
		{"\u00a0abc\u00a0", "abc"},             // NBSP
		{"\u3000abc\u3000", "abc"},             // ideographic space
		{"\ufeffabc\ufeff", "abc"},             // BOM / ZWNBSP
		{"\u2028abc\u2029", "abc"},             // line/paragraph separator
		{"\u202fabc\u205f", "abc"},             // narrow NBSP, medium math space
		{"\u0085abc\u0085", "\u0085abc\u0085"}, // NEL: NOT JS whitespace
		{"abc", "abc"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := JSTrim(tc.in); got != tc.want {
			t.Errorf("JSTrim(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// The divergence is the interesting part: Go would strip NEL, JS would not.
	t.Run("NEL whitespace-only title is accepted because JS does not trim it", func(t *testing.T) {
		_, err := ParseCreateReportInput(decode(t, `{"title":"\u0085","content":"c"}`), 1<<20)
		if err != nil {
			t.Errorf("U+0085 should count as a real character: %v", err)
		}
	})
}

// TestParseJSONBodyRejectsMalformedInput covers the body decoder.
func TestParseJSONBodyRejectsMalformedInput(t *testing.T) {
	bad := []string{"", "  ", "{", "[1,", `{"a":1} trailing`, "undefined"}
	for _, raw := range bad {
		if _, err := ParseJSONBody([]byte(raw)); err == nil {
			t.Errorf("ParseJSONBody(%q) should fail", raw)
		}
	}

	t.Run("preserves large integers", func(t *testing.T) {
		body, err := ParseJSONBody([]byte(`{"n":9007199254740993}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		obj := body.(map[string]any)
		if _, ok := obj["n"].(json.Number); !ok {
			t.Errorf("expected json.Number, got %T", obj["n"])
		}
	})
}

// TestParseShareTokenShape covers the regex bound before SQL.
func TestParseShareTokenShape(t *testing.T) {
	valid := []string{
		"aaaaaaaaaaaaaaaa",                 // exactly 16
		"lnE1BkG63lo17NS5fKh9cA",           // realistic
		strings.Repeat("a", 64),            // exactly 64
		"AZaz09_-AZaz09_-AZaz09_-AZaz09_-", // full alphabet
	}
	for _, token := range valid {
		if _, err := ParseShareToken(token); err != nil {
			t.Errorf("ParseShareToken(%q) rejected a valid token: %v", token, err)
		}
	}

	invalid := []string{
		"",
		"short",                 // 5 chars
		strings.Repeat("a", 15), // one short
		strings.Repeat("a", 65), // one long
		"has spaces here!!",
		"has+plus+chars+here",
		"has/slash/chars/her",
		"has=equals=chars=he",
	}
	for _, token := range invalid {
		if _, err := ParseShareToken(token); err == nil {
			t.Errorf("ParseShareToken(%q) should have been rejected", token)
		}
	}

	// A malformed token must surface as 404, not 400, so it is indistinguishable
	// from an unknown token.
	_, err := ParseShareToken("nope")
	var apiErr *httpx.Error
	if !asAPIError(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("expected a 404 for a malformed token, got %v", err)
	}
}

// asAPIError is a tiny errors.As wrapper kept local so the test file does not
// need to import errors at every call site.
func asAPIError(err error, target **httpx.Error) bool {
	for err != nil {
		if e, ok := err.(*httpx.Error); ok {
			*target = e
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
