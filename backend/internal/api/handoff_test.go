package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zeroicey/teleport/backend/internal/config"
	"github.com/zeroicey/teleport/backend/internal/store"
	"github.com/zeroicey/teleport/backend/internal/views"
)

// envelopeCode returns the error code of an envelope response.
func envelopeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	_, _, code := decodeEnvelope(t, rec)
	return code
}

// createHandoff drives the admin endpoint and returns the decoded payload.
func createHandoff(t *testing.T, h http.Handler, cookie map[string]string, body string) map[string]any {
	t.Helper()
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/key-handoffs", body, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create handoff: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	ok, data, _ := decodeEnvelope(t, rec)
	if !ok {
		t.Fatalf("create handoff: ok=false: %s", rec.Body.String())
	}
	return data
}

// TestKeyHandoffIsSessionGated keeps the issuance surface on the human side of
// the fence: no agent credential may mint one, and neither may an anonymous
// caller.
func TestKeyHandoffIsSessionGated(t *testing.T) {
	h, _ := testServer(t)
	body := `{"label":"A姐"}`

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no credentials", nil, http.StatusUnauthorized},
		{"agent bearer token", agentHeaders(), http.StatusUnauthorized},
		{"dashboard session", map[string]string{"Cookie": login(t, h)}, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/key-handoffs", body, tc.headers)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestKeyHandoffLinkCarriesTheSecretOnlyInTheFragment is the security core of
// the feature.
//
// The claim secret is what turns the link into a credential. It must therefore
// live in the URL *fragment*, which browsers never send to a server: putting it
// in the path or query would write it into access logs and into the Referer of
// anything the page loads.
func TestKeyHandoffLinkCarriesTheSecretOnlyInTheFragment(t *testing.T) {
	h, _ := testServer(t)
	data := createHandoff(t, h, map[string]string{"Cookie": login(t, h)},
		`{"label":"A姐","expiresInHours":24,"note":"月报发布用"}`)

	id, _ := data["id"].(string)
	secret, _ := data["claim_secret"].(string)
	rawURL, _ := data["url"].(string)
	prompt, _ := data["prompt"].(string)

	if id == "" || secret == "" || rawURL == "" {
		t.Fatalf("missing id/claim_secret/url in %s", data)
	}
	if data["label"] != "A姐" {
		t.Errorf("label = %v, want A姐", data["label"])
	}
	if data["key_expires_in_hours"] != float64(24) {
		t.Errorf("key_expires_in_hours = %v, want 24", data["key_expires_in_hours"])
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("hand-off url is not a URL: %v", err)
	}
	if parsed.Fragment != secret {
		t.Errorf("fragment = %q, want the bare claim secret", parsed.Fragment)
	}
	// The client reads the fragment with a bare `slice(1)`, so a key/value shape
	// (`#k=<secret>`) would render a page whose claim button never appears. A
	// real Chromium run caught exactly that drift once; this is the tripwire
	// that keeps the two sides from diverging again.
	if strings.Contains(parsed.Fragment, "=") {
		t.Errorf("fragment %q looks like key=value; the page client expects the bare secret",
			parsed.Fragment)
	}
	if strings.Contains(rawURL, "?") {
		t.Errorf("url %q has a query string; the secret must not travel in a logged position", rawURL)
	}
	// The parts a server actually sees must be clean of it.
	serverBound := parsed.Scheme + "://" + parsed.Host + parsed.Path + "?" + parsed.RawQuery
	if strings.Contains(serverBound, secret) {
		t.Fatalf("claim secret appears in the server-bound part of the URL: %s", serverBound)
	}
	if !strings.HasSuffix(parsed.Path, "/k/"+id) {
		t.Errorf("path = %q, want it to end with /k/{id}", parsed.Path)
	}
	if parsed.RawQuery != "" {
		t.Errorf("query = %q, want empty (a query is logged)", parsed.RawQuery)
	}

	// The prompt is the agent-facing path and therefore *must* carry it.
	if !strings.Contains(prompt, secret) {
		t.Error("prompt does not contain the claim secret, so an agent cannot claim")
	}
	if !strings.Contains(prompt, id) || !strings.Contains(prompt, "/ai.md") {
		t.Errorf("prompt does not tell an agent where to exchange it: %s", prompt)
	}

	// Expiry of the link is the application TTL, not the claim window: a link
	// forwarded today has to survive being opened tomorrow.
	deadline, _ := data["link_expires_at"].(float64)
	want := time.Now().Add(24 * time.Hour).UnixMilli()
	if diff := want - int64(deadline); diff < -60_000 || diff > 60_000 {
		t.Errorf("link_expires_at = %d, want ~%d (KEY_APPLICATION_TTL)", int64(deadline), want)
	}
}

// TestKeyHandoffClaimFlow walks the whole point of the feature: generate, hand
// over, claim — with no second administrator action in between.
func TestKeyHandoffClaimFlow(t *testing.T) {
	h, _ := testServer(t)
	data := createHandoff(t, h, map[string]string{"Cookie": login(t, h)}, `{"label":"A姐"}`)

	id, _ := data["id"].(string)
	secret, _ := data["claim_secret"].(string)

	// Immediately claimable: the row was created approved, so no approval step
	// exists between generating the link and using it.
	rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	_, payload, _ := decodeEnvelope(t, rec)
	key, _ := payload["key"].(map[string]any)
	token, _ := key["token"].(string)
	if token == "" {
		t.Fatalf("claim returned no token: %s", rec.Body.String())
	}
	if key["name"] != "A姐" {
		t.Errorf("claimed key name = %v, want the hand-off label", key["name"])
	}

	// The credential works.
	rec = do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "",
		map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusOK {
		t.Fatalf("the claimed token is not usable: %d %s", rec.Code, rec.Body.String())
	}

	// A hand-off link is single-use: the second reader gets the same 404 as an
	// unknown id, so the link cannot be replayed by whoever else sees it.
	rec = do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))
	if rec.Code != http.StatusNotFound {
		t.Errorf("second claim: status = %d, want 404", rec.Code)
	}
	if code := envelopeCode(t, rec); code != "not_found" {
		t.Errorf("second claim code = %q, want not_found", code)
	}
}

// TestKeyHandoffPageStates covers what the recipient actually sees.
func TestKeyHandoffPageStates(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	t.Run("open link offers a claim", func(t *testing.T) {
		data := createHandoff(t, h, cookie, `{"label":"A姐","expiresInHours":24}`)
		id, _ := data["id"].(string)

		rec := do(t, h, http.MethodGet, testPrefix+"/k/"+id, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		page := rec.Body.String()

		if got := rec.Header().Get("Cache-Control"); got != "no-store, must-revalidate" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != views.ContentSecurityPolicy {
			t.Errorf("CSP = %q, want the share page policy (script-src 'self')", got)
		}
		if !strings.Contains(page, `data-handoff-id="`+id+`"`) {
			t.Error("page does not carry the hand-off id for the client")
		}
		if !strings.Contains(page, `data-app-base="`+cfg.AppBaseURL()+`"`) {
			t.Error("page does not carry the absolute app base")
		}
		// The client composes the API path itself. Handing it the API root once
		// produced `/api/api/...` and every claim 404'd, so assert the shape the
		// client actually consumes rather than trusting the attribute name.
		if strings.Contains(page, `data-app-base="`+cfg.AppBaseURL()+`/api"`) {
			t.Error("page carries the API base where the client expects the app base")
		}
		if !strings.Contains(page, `data-handoff-spent="0"`) {
			t.Error("an open link must not be marked spent")
		}
		if !strings.Contains(page, "noindex") {
			t.Error("page is indexable")
		}
		// The secret cannot be in the HTML: the server never receives it.
		secret, _ := data["claim_secret"].(string)
		if strings.Contains(page, secret) {
			t.Fatal("the claim secret appears in the server-rendered page")
		}
		if !strings.Contains(page, cfg.AppBaseURL()+"/ai.md") {
			t.Error("page does not point the recipient at the guide")
		}
	})

	t.Run("claimed link explains itself", func(t *testing.T) {
		data := createHandoff(t, h, cookie, `{"label":"B姐"}`)
		id, _ := data["id"].(string)
		secret, _ := data["claim_secret"].(string)
		do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
			claimHeaders(secret))

		rec := do(t, h, http.MethodGet, testPrefix+"/k/"+id, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a person, not an API client): %s", rec.Code, rec.Body.String())
		}
		page := rec.Body.String()
		if !strings.Contains(page, `data-handoff-spent="1"`) {
			t.Error("a claimed link is not marked spent")
		}
		if !strings.Contains(page, "已经被领取过了") {
			t.Error("page does not tell the recipient the link was already used")
		}
	})

	t.Run("expired link explains itself", func(t *testing.T) {
		// Drive the link past its deadline through the same clock the server
		// uses, rather than sleeping for 24 hours.
		h2, _ := testServerWith(t, func(c *config.Config) { c.KeyApplicationTTL = time.Millisecond })
		cookie2 := map[string]string{"Cookie": login(t, h2)}
		data2 := createHandoff(t, h2, cookie2, `{"label":"D姐"}`)
		id2, _ := data2["id"].(string)
		time.Sleep(5 * time.Millisecond)

		rec := do(t, h2, http.MethodGet, testPrefix+"/k/"+id2, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		page := rec.Body.String()
		if !strings.Contains(page, `data-handoff-spent="1"`) {
			t.Error("an expired link is not marked spent")
		}
		if !strings.Contains(page, "过期") {
			t.Error("page does not say the link expired")
		}

		// And the underlying claim agrees: 410, not a silent 404.
		secret2, _ := data2["claim_secret"].(string)
		rec = do(t, h2, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id2, "",
			claimHeaders(secret2))
		if rec.Code != http.StatusGone {
			t.Errorf("claiming an expired link: status = %d, want 410", rec.Code)
		}
	})

	t.Run("unknown id still renders a page", func(t *testing.T) {
		rec := do(t, h, http.MethodGet,
			testPrefix+"/k/00000000-0000-4000-8000-000000000000", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a person followed a link): %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "无效") {
			t.Error("page does not explain that the link is invalid")
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want HTML", ct)
		}
	})

	t.Run("truncated link does not fall through to the SPA", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/k/", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "data-handoff-id") {
			t.Error("bare /k/ did not render the hand-off page")
		}
	})
}

// TestKeyHandoffValidation covers the input contract.
func TestKeyHandoffValidation(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"missing label", `{}`, http.StatusBadRequest, "bad_request"},
		{"blank label", `{"label":"   "}`, http.StatusBadRequest, "bad_request"},
		{"negative hours", `{"label":"A姐","expiresInHours":-1}`, http.StatusBadRequest, "bad_request"},
		{"never expires is allowed", `{"label":"A姐","expiresInHours":0}`, http.StatusCreated, ""},
		{"label too long", `{"label":"` + strings.Repeat("x", 101) + `"}`, http.StatusBadRequest, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/key-handoffs", tc.body, cookie)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.code != "" {
				if code := envelopeCode(t, rec); code != tc.code {
					t.Errorf("code = %q, want %q", code, tc.code)
				}
			}
		})
	}
}

// TestKeyHandoffNeverExposesAHash keeps the response envelope free of the
// stored hash, matching every other key surface.
func TestKeyHandoffNeverExposesAHash(t *testing.T) {
	h, _ := testServer(t)
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/key-handoffs",
		`{"label":"A姐"}`, map[string]string{"Cookie": login(t, h)})
	assertNoHashLeak(t, "handoff response", rec.Body.String())

	// The page must not leak it either.
	_, data, _ := decodeEnvelope(t, rec)
	id, _ := data["id"].(string)
	page := do(t, h, http.MethodGet, testPrefix+"/k/"+id, "", nil)
	assertNoHashLeak(t, "handoff page", page.Body.String())
}

// TestKeyHandoffDoesNotCreateAKeyUntilClaimed pins the "approval adds no key
// row" contract that the whole derivation design rests on.
func TestKeyHandoffDoesNotCreateAKeyUntilClaimed(t *testing.T) {
	h, cfg := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	data := createHandoff(t, h, cookie, `{"label":"A姐"}`)
	id, _ := data["id"].(string)
	secret, _ := data["claim_secret"].(string)

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	st := store.New(db)

	keys, err := st.ListAgentKeys()
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("generating a hand-off created %d key row(s); the credential must not exist yet", len(keys))
	}
	app, err := st.GetApplication(id)
	if err != nil {
		t.Fatalf("load application: %v", err)
	}
	if app == nil || app.Status != "approved" {
		t.Fatalf("application = %+v, want an approved row", app)
	}
	if app.IssuedKeyID != "" {
		t.Errorf("issued_key_id = %q, want empty before the claim", app.IssuedKeyID)
	}

	do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))

	keys, err = st.ListAgentKeys()
	if err != nil {
		t.Fatalf("list keys after claim: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("after the claim there are %d keys, want exactly 1", len(keys))
	}
	if keys[0].Name != "A姐" {
		t.Errorf("claimed key name = %q, want the hand-off label", keys[0].Name)
	}
}
