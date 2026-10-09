package api

import (
	"encoding/json"
	"math"
	"regexp"

	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/httpx"
	"github.com/zeroicey/teleport/backend/internal/validate"
	"github.com/zeroicey/teleport/backend/internal/views"
)

// shareTokenShape bounds a token before it reaches SQL. Mirrors the original
// /^[A-Za-z0-9_-]{16,64}$/.
var shareTokenShape = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func isShareTokenShape(raw string) bool { return shareTokenShape.MatchString(raw) }

// validateJSON decodes a JSON body and rejects trailing content.
func validateJSON(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, httpx.BadRequest("Request body is required", nil)
	}
	return validate.ParseJSONBody(raw)
}

func parseCreateReport(body any, maxContentBytes int64) (domain.CreateReportInput, error) {
	return validate.ParseCreateReportInput(body, maxContentBytes)
}

func requireString(value any, field string, min, max int) (string, error) {
	return validate.RequireString(value, field, min, max)
}

// readHours extracts `expiresInHours` from an optional request body. A missing
// body, missing field or null value means "never expires" (0), matching the
// original readHours helper.
func readHours(body map[string]any) (float64, error) {
	if body == nil {
		return 0, nil
	}
	value, ok := body["expiresInHours"]
	if !ok || value == nil {
		return 0, nil
	}
	hours, ok := jsNumber(value)
	if !ok || hours < 0 {
		return 0, httpx.BadRequest("`expiresInHours` must be a non-negative number", nil)
	}
	return hours, nil
}

// jsNumber reproduces JavaScript's Number() coercion for the JSON values that
// can reach these fields.
func jsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case string:
		s := validate.JSTrim(n)
		if s == "" {
			return 0, false
		}
		f, err := json.Number(s).Float64()
		if err != nil {
			return 0, false
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// renderSharePage renders the public share page for a resolved token.
//
// The asset base points at the frontend origin, because that is where the
// static build (and therefore /assets/share.js) is served from. The page is
// normally delivered through that same origin, so the reference stays
// same-origin and satisfies `script-src 'self'`.
func renderSharePage(s *Server, resolution *domain.Resolution) string {
	return views.RenderSharePage(views.SharePageOptions{
		Report:       *resolution.Report,
		Share:        *resolution.Share,
		AssetBaseURL: s.cfg.AssetBaseURL,
	})
}
