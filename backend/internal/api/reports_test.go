package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zeroicey/teleport/backend/internal/config"
	"github.com/zeroicey/teleport/backend/internal/store"
)

// reportField reads one field of a report through the agent read endpoint.
func reportField(t *testing.T, h http.Handler, token, id, field string) any {
	t.Helper()
	rec := do(t, h, http.MethodGet, testPrefix+"/api/reports/"+id, "", bearerHeaders(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("read report %s: status = %d: %s", id, rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	return data[field]
}

// reportExists asks the database directly, bypassing the API's ownership rules,
// so a test can prove that a refused request wrote nothing rather than merely
// that it returned 404.
func reportExists(t *testing.T, cfg *config.Config, id string) bool {
	t.Helper()
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reports WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	return n > 0
}

func shareTokenCount(t *testing.T, cfg *config.Config, reportID string) int {
	t.Helper()
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM share_tokens WHERE report_id = ?`, reportID).Scan(&n); err != nil {
		t.Fatalf("count share tokens: %v", err)
	}
	return n
}

// TestUpdateReportIsInPlaceAndVisibleThroughTheExistingLink is the core promise
// of PATCH: the report keeps its identity, so every link already handed out
// starts serving the new content instead of breaking.
func TestUpdateReportIsInPlaceAndVisibleThroughTheExistingLink(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "editor")

	id, share := publishReport(t, h, token, "before")
	// Deliberately no sleep here. The update may well land in the same
	// millisecond as the publish, and updated_at must still move forward — the
	// store guarantees it with MAX(now, updated_at + 1). Sleeping would hide a
	// regression in exactly that guarantee, which is how the original
	// second-granularity trigger bug went unnoticed.

	rec := do(t, h, http.MethodPatch, testPrefix+"/api/reports/"+id,
		`{"content":"# after","title":"after"}`, bearerHeaders(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status = %d: %s", rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	if got := data["title"]; got != "after" {
		t.Errorf("title = %v, want after", got)
	}
	if got := data["id"]; got != id {
		t.Errorf("id = %v, want %s (update must not mint a new report)", got, id)
	}

	createdAt, _ := data["created_at"].(float64)
	updatedAt, _ := data["updated_at"].(float64)
	if updatedAt <= createdAt {
		t.Errorf("updated_at = %v did not move past created_at = %v (it must be strictly increasing, "+
			"even when the update lands in the same millisecond as the publish)", updatedAt, createdAt)
	}

	// The whole point: the link that was already handed out now serves the new
	// body. A share page is rendered from the report row, so no token work is
	// needed — this asserts that stays true.
	page := do(t, h, http.MethodGet, testPrefix+"/s/"+share, "", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("share page: status = %d", page.Code)
	}
	if body := page.Body.String(); !strings.Contains(body, "after") {
		t.Error("share page still serves the old content after an update")
	}
	if cfg.DefaultShareHours == 0 {
		t.Fatal("fixture is not production-shaped")
	}
}

// TestPatchRefusesFieldsItWouldOtherwiseSilentlyIgnore covers the failure mode
// this design exists to prevent: a caller sends a field name the server does not
// recognise, gets a 200, and believes the report changed.
//
// Every case below pairs the rejected field with a *valid* one on purpose. With
// only the bad field present the empty-patch rule would also answer 400, and the
// test would pass while proving nothing about unknown fields — which is exactly
// what a falsification run showed: deleting the unknown-field check left the
// first version of this test green. The valid field is what makes the assertion
// load-bearing, and it doubles as the thing that must NOT be applied.
func TestPatchRefusesFieldsItWouldOtherwiseSilentlyIgnore(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "typo-agent")
	id, _ := publishReport(t, h, token, "keep me")

	for _, tc := range []struct{ name, body string }{
		// The single most likely mistake: request bodies are camelCase but
		// responses are snake_case, so the tempting spelling of the response
		// field is the wrong one to send.
		{"capitalised content", `{"content":"# applied?","Content":"# nope"}`},
		{"snake_case updated_at", `{"content":"# applied?","updated_at":1}`},
		// Ownership is a security boundary, not data. A caller must not be able
		// to hand its report to another key or claim one by patching.
		{"owner_key_id", `{"title":"hijacked","owner_key_id":"someone-else"}`},
		{"unknown field", `{"title":"hijacked","nonsense":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPatch, testPrefix+"/api/reports/"+id, tc.body, bearerHeaders(token))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (a silently ignored field is a silent wrong answer): %s",
					rec.Code, rec.Body.String())
			}
			_, _, code := decodeEnvelope(t, rec)
			if code != "bad_request" {
				t.Errorf("error code = %q, want bad_request", code)
			}
			// All-or-nothing: the valid field must not have been applied either,
			// or a caller could smuggle a change past the rejected key.
			if got := reportField(t, h, token, id, "content"); got == "# applied?" {
				t.Error("the valid field was applied despite the request being rejected")
			}
			if got := reportField(t, h, token, id, "title"); got == "hijacked" {
				t.Error("the valid field was applied despite the request being rejected")
			}
		})
	}

	// The refused requests must not have bumped updated_at either, or the share
	// page would advertise edits that never happened.
	if got := reportField(t, h, token, id, "updated_at"); got != reportField(t, h, token, id, "created_at") {
		t.Errorf("a rejected patch moved updated_at: %v", got)
	}
	if !reportExists(t, cfg, id) {
		t.Error("report vanished")
	}
}

// TestEmptyPatchIsRefused: `{}` would bump updated_at and make the share page
// say "Updated" about a report nobody edited.
func TestEmptyPatchIsRefused(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "noop-agent")
	id, _ := publishReport(t, h, token, "unchanged")

	for _, body := range []string{`{}`, `{"title":null}`, `{"metadata":null}`} {
		rec := do(t, h, http.MethodPatch, testPrefix+"/api/reports/"+id, body, bearerHeaders(token))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("patch %s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestPatchValidatesLikeCreate: PATCH must not become a way to smuggle in a
// value that POST would have refused.
func TestPatchValidatesLikeCreate(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "validator-agent")
	id, _ := publishReport(t, h, token, "guarded")

	for _, tc := range []struct{ name, body string }{
		{"empty content", `{"content":""}`},
		{"empty title", `{"title":""}`},
		{"bad category", `{"category":"Not Allowed"}`},
		{"bad format", `{"format":"pdf"}`},
		{"metadata not an object", `{"metadata":"nope"}`},
		{"content too large", `{"content":"` + string(make([]byte, 1)) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPatch, testPrefix+"/api/reports/"+id, tc.body, bearerHeaders(token))
			if rec.Code < 400 {
				t.Errorf("status = %d, want a 4xx: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMetadataReplacesWholesale documents the one non-obvious PATCH semantic:
// sending metadata replaces the object, and {} clears it. Merging was rejected
// because it cannot express "remove this key".
func TestMetadataReplacesWholesale(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "meta-agent")
	rec := do(t, h, http.MethodPost, testPrefix+"/api/reports",
		`{"title":"meta","content":"# meta","metadata":{"keep":"no","drop":"yes"}}`,
		bearerHeaders(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d: %s", rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	id, _ := data["id"].(string)

	rec = do(t, h, http.MethodPatch, testPrefix+"/api/reports/"+id,
		`{"metadata":{"fresh":"yes"}}`, bearerHeaders(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch metadata: %d: %s", rec.Code, rec.Body.String())
	}
	_, data, _ = decodeEnvelope(t, rec)
	meta, _ := data["metadata"].(map[string]any)
	if _, stale := meta["drop"]; stale {
		t.Error("metadata was merged; the old key survived a replacement")
	}
	if meta["fresh"] != "yes" {
		t.Errorf("metadata = %v, want the replacement object", meta)
	}
}

// TestUpdateAndDeleteRespectOwnership is the security test. Both operations must
// answer 404 for a report belonging to another key, and — the part that matters
// — must not have written anything while deciding.
func TestUpdateAndDeleteRespectOwnership(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	tokenA, _ := mintKey(t, h, cookie, "owner-a")
	tokenB, _ := mintKey(t, h, cookie, "intruder-b")

	id, share := publishReport(t, h, tokenA, "A's private report")
	before := reportField(t, h, tokenA, id, "content")

	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			body := ""
			if method == http.MethodPatch {
				body = `{"content":"# defaced"}`
			}
			rec := do(t, h, method, testPrefix+"/api/reports/"+id, body, bearerHeaders(tokenB))
			// 404 rather than 403: a non-owner must not be able to confirm the
			// report exists. Same rule as reading it or revoking its links.
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
			}
			if got := reportField(t, h, tokenA, id, "content"); got != before {
				t.Errorf("a refused %s changed the report: %v", method, got)
			}
			if !reportExists(t, cfg, id) {
				t.Errorf("a refused %s deleted the report", method)
			}
			if got := shareTokenCount(t, cfg, id); got == 0 {
				t.Errorf("a refused %s removed the share tokens", method)
			}
		})
	}

	// Root's reports store owner_key_id = "", which a named key must never match.
	rootID, _ := publishReport(t, h, testAgentSecret, "root's report")
	if rec := do(t, h, http.MethodDelete, testPrefix+"/api/reports/"+rootID, "", bearerHeaders(tokenA)); rec.Code != http.StatusNotFound {
		t.Errorf("named key deleting a root report = %d, want 404", rec.Code)
	}
	if !reportExists(t, cfg, rootID) {
		t.Error("a named key deleted a root-owned report")
	}

	// And the link is still live, because nothing was deleted.
	if rec := do(t, h, http.MethodGet, testPrefix+"/s/"+share, "", nil); rec.Code != http.StatusOK {
		t.Errorf("share link after refused deletes = %d, want 200", rec.Code)
	}
}

// TestDeleteReportCascadesToItsShareLinks guards the schema's ON DELETE CASCADE.
//
// SQLite enforces foreign keys only when the pragma is on, so a missing
// `_pragma=foreign_keys(1)` would turn that cascade into dead code: the report
// would go and its tokens would stay behind forever, invisible to every query
// that joins through reports. Asserting on the row count directly is what makes
// this a guard rather than a coincidence of the join.
func TestDeleteReportCascadesToItsShareLinks(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "deleter")
	id, share := publishReport(t, h, token, "to be deleted")

	// A second link on the same report, to prove the cascade is not limited to
	// the auto-created token.
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/reports/"+id+"/shares", `{}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create extra share: %d: %s", rec.Code, rec.Body.String())
	}
	if got := shareTokenCount(t, cfg, id); got != 2 {
		t.Fatalf("share tokens before delete = %d, want 2", got)
	}

	if rec := do(t, h, http.MethodDelete, testPrefix+"/api/reports/"+id, "", bearerHeaders(token)); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d: %s", rec.Code, rec.Body.String())
	}

	if reportExists(t, cfg, id) {
		t.Error("report still exists after a successful delete")
	}
	if got := shareTokenCount(t, cfg, id); got != 0 {
		t.Errorf("orphaned share tokens after delete = %d, want 0 (ON DELETE CASCADE did not fire)", got)
	}
	// Both links must now be gone, and a gone link is 404 — the same answer an
	// unknown token gets, so deletion leaks nothing about what used to be here.
	for _, tok := range []string{share} {
		if rec := do(t, h, http.MethodGet, testPrefix+"/s/"+tok, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("share link %s after delete = %d, want 404", tok, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodGet, testPrefix+"/api/reports/"+id, "", bearerHeaders(token)); rec.Code != http.StatusNotFound {
		t.Errorf("reading a deleted report = %d, want 404", rec.Code)
	}
}

// TestDeleteIsNotIdempotentByDesign: the second DELETE answers 404, matching how
// every other "no such thing" case in this API answers, rather than reporting a
// success for a row that was not there.
func TestDeleteIsNotIdempotentByDesign(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "twice-agent")
	id, _ := publishReport(t, h, token, "delete me twice")

	if rec := do(t, h, http.MethodDelete, testPrefix+"/api/reports/"+id, "", bearerHeaders(token)); rec.Code != http.StatusOK {
		t.Fatalf("first delete: %d", rec.Code)
	}
	rec := do(t, h, http.MethodDelete, testPrefix+"/api/reports/"+id, "", bearerHeaders(token))
	if rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
	_, _, code := decodeEnvelope(t, rec)
	if code != "not_found" {
		t.Errorf("error code = %q, want not_found", code)
	}
}

// TestDashboardMayUpdateAndDeleteAnyReport covers the god-view ruling: the
// human's session is not subject to ownership, because the dashboard is the
// deployment owner's own surface.
func TestDashboardMayUpdateAndDeleteAnyReport(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "managed-agent")
	id, _ := publishReport(t, h, token, "managed by the dashboard")

	rec := do(t, h, http.MethodPatch, testPrefix+"/api/admin/reports/"+id,
		`{"title":"renamed by the human"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin patch: %d: %s", rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	if data["title"] != "renamed by the human" {
		t.Errorf("title = %v, want the dashboard's edit", data["title"])
	}
	// The dashboard edit must be visible to the owning agent.
	if got := reportField(t, h, token, id, "title"); got != "renamed by the human" {
		t.Errorf("agent sees title = %v after a dashboard edit", got)
	}

	if rec := do(t, h, http.MethodDelete, testPrefix+"/api/admin/reports/"+id, "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("admin delete: %d: %s", rec.Code, rec.Body.String())
	}
	if reportExists(t, cfg, id) {
		t.Error("dashboard delete did not remove the report")
	}
}

// TestAdminReportMutationsStillNeedASession: the dashboard routes must not be
// reachable without the session cookie, and an agent bearer token must not be a
// substitute for it.
func TestAdminReportMutationsStillNeedASession(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "not-an-admin")
	id, _ := publishReport(t, h, token, "protected from the agent route")

	for _, hdr := range []map[string]string{nil, bearerHeaders(token), agentHeaders()} {
		rec := do(t, h, http.MethodDelete, testPrefix+"/api/admin/reports/"+id, "", hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("admin delete with headers %v = %d, want 401", hdr, rec.Code)
		}
	}
	if !reportExists(t, cfg, id) {
		t.Error("an unauthenticated request deleted a report")
	}
}

// TestUpdateReportsWrongVerbIsRefused: PUT is not PATCH, and a PUT that quietly
// behaved like a full replacement would be a surprise.
func TestUpdateReportsWrongVerbIsRefused(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, _ := mintKey(t, h, cookie, "verb-agent")
	id, _ := publishReport(t, h, token, "verb test")

	rec := do(t, h, http.MethodPut, testPrefix+"/api/reports/"+id, `{"title":"put"}`, bearerHeaders(token))
	if rec.Code == http.StatusOK {
		t.Error("PUT on a report succeeded; only PATCH should mutate")
	}
	if got := reportField(t, h, token, id, "title"); got != "verb test" {
		t.Errorf("title = %v after a refused PUT, want it unchanged", got)
	}
}
