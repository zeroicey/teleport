// Package validate implements request payload validation.
//
// It is deliberately hand-rolled rather than schema-driven: the accepted
// surface is small, and hand-rolling keeps error messages precise so agents get
// actionable feedback.
//
// Fidelity note: the previous implementation ran on the Workers runtime, so its
// behaviour depended on JavaScript semantics. Two of those differ from the
// obvious Go equivalent and are reproduced here deliberately, because they are
// observable through the API:
//
//   - `String.prototype.trim()` strips the ECMAScript WhiteSpace and
//     LineTerminator sets, which include the Unicode space separators but
//     EXCLUDE U+0085 (NEL). Go's strings.TrimSpace uses unicode.IsSpace, which
//     INCLUDES U+0085. jsTrim below implements the ECMAScript set.
//   - `str.length` counts UTF-16 code units, not runes or bytes. A title of 300
//     emoji is 600 units and must be rejected, so lengths are measured in
//     UTF-16 code units.
package validate

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/httpx"
)

// TITLE_MAX is the maximum title length in UTF-16 code units.
const TITLE_MAX = 300

// AUTO_SHARE_HOURS_MAX is ten years, matching the original limit.
const AUTO_SHARE_HOURS_MAX = 24 * 365 * 10

var (
	formatSet  = map[string]bool{"markdown": true, "html": true}
	categoryRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// shareTokenRE bounds a token before it ever reaches SQL.
	shareTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
)

// ParseCreateReportInput validates the body of POST /api/reports.
func ParseCreateReportInput(body any, maxContentBytes int64) (domain.CreateReportInput, error) {
	var out domain.CreateReportInput

	obj, ok := body.(map[string]any)
	if !ok {
		return out, httpx.BadRequest("Request body must be a JSON object", nil)
	}

	title, err := RequireString(obj["title"], "title", 1, TITLE_MAX)
	if err != nil {
		return out, err
	}

	content, ok := obj["content"].(string)
	if !ok || content == "" {
		return out, httpx.BadRequest("`content` is required and must be a non-empty string",
			map[string]any{"field": "content"})
	}
	// Measure bytes, not code units: SQLite row limits are byte-based and CJK
	// content is ~3 bytes per character.
	contentBytes := int64(len(content))
	if contentBytes > maxContentBytes {
		return out, httpx.PayloadTooLarge(
			"`content` is " + strconv.FormatInt(contentBytes, 10) + " bytes, exceeding the " +
				strconv.FormatInt(maxContentBytes, 10) + " byte limit")
	}

	category := "general"
	if v := obj["category"]; present(v) {
		s, ok := v.(string)
		if !ok || !categoryRE.MatchString(s) {
			return out, httpx.BadRequest(
				"`category` must match /^[a-z0-9][a-z0-9_-]{0,31}$/ (e.g. pentest, architecture, progress)",
				map[string]any{"field": "category"})
		}
		category = s
	}

	format := domain.FormatMarkdown
	if v := obj["format"]; present(v) {
		s, ok := v.(string)
		if !ok || !formatSet[s] {
			return out, httpx.BadRequest("`format` must be one of: markdown, html",
				map[string]any{"field": "format"})
		}
		format = domain.ReportFormat(s)
	}

	var metadata map[string]any
	if v := obj["metadata"]; present(v) {
		m, ok := v.(map[string]any)
		if !ok {
			return out, httpx.BadRequest("`metadata` must be a JSON object",
				map[string]any{"field": "metadata"})
		}
		metadata = m
	}

	var autoShareHours *float64
	if v := obj["autoShareHours"]; present(v) {
		n, ok := jsToNumber(v)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > AUTO_SHARE_HOURS_MAX {
			return out, httpx.BadRequest(
				"`autoShareHours` must be a number between 0 and 87600 (0 = never expires)",
				map[string]any{"field": "autoShareHours"})
		}
		autoShareHours = &n
	}

	out = domain.CreateReportInput{
		Title:          title,
		Category:       category,
		Format:         format,
		Content:        content,
		Metadata:       metadata,
		AutoShareHours: autoShareHours,
	}
	return out, nil
}

// present reports whether an optional JSON field carries a meaningful value.
//
// The original checked `!== undefined && !== null && !== ”`, so an empty string
// counts as absent and falls back to the default.
func present(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return s != ""
	}
	return true
}

// RequireString reads a required, trimmed string bounded by UTF-16 length.
func RequireString(value any, field string, min, max int) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", httpx.BadRequest("`"+field+"` is required and must be a string",
			map[string]any{"field": field})
	}
	trimmed := JSTrim(s)
	n := UTF16Len(trimmed)
	if n < min || n > max {
		return "", httpx.BadRequest(
			"`"+field+"` must be between "+strconv.Itoa(min)+" and "+strconv.Itoa(max)+" characters",
			map[string]any{"field": field})
	}
	return trimmed, nil
}

// ParseShareToken validates a share token's shape before it reaches SQL.
func ParseShareToken(raw string) (string, error) {
	if !shareTokenRE.MatchString(raw) {
		return "", httpx.NotFound("Report or share link not found")
	}
	return raw, nil
}

// ParseJSONBody decodes a request body, preserving number formatting exactly by
// using json.Number so a large integer in `metadata` survives a round-trip.
func ParseJSONBody(raw []byte) (any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, httpx.BadRequest("Request body is required", nil)
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		return nil, httpx.BadRequest("Request body is not valid JSON", nil)
	}
	// Reject trailing content, which JSON.parse also rejects.
	if decoder.More() {
		return nil, httpx.BadRequest("Request body is not valid JSON", nil)
	}
	return out, nil
}

// UTF16Len returns the length of s in UTF-16 code units, matching
// JavaScript's String.prototype.length.
func UTF16Len(s string) int {
	if isASCII(s) {
		return len(s)
	}
	return len(utf16.Encode([]rune(s)))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// JSTrim removes leading and trailing ECMAScript whitespace.
//
// The ECMAScript set is WhiteSpace + LineTerminator: TAB, VT, FF, SP, NBSP,
// BOM/ZWNBSP, the Unicode Zs category (which includes U+1680, U+2000-U+200A,
// U+202F, U+205F, U+3000), plus LF, CR, LS and PS. Notably it excludes U+0085
// (NEL), which Go's unicode.IsSpace would strip.
func JSTrim(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff',
		'\n', '\r', '\u2028', '\u2029',
		'\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004',
		'\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a',
		'\u202f', '\u205f', '\u3000':
		return true
	}
	return false
}

// jsToNumber reproduces JavaScript's Number() coercion for the JSON value
// types that can appear here.
func jsToNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		s := JSTrim(n)
		if s == "" {
			return 0, true
		}
		// Handle the special forms Number() accepts.
		switch s {
		case "Infinity", "+Infinity":
			return math.Inf(1), true
		case "-Infinity":
			return math.Inf(-1), true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
		// Hexadecimal literals are accepted by Number().
		if len(s) > 2 && (s[0:2] == "0x" || s[0:2] == "0X") {
			if i, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
				return float64(i), true
			}
		}
		return math.NaN(), true
	}
	return math.NaN(), true
}
