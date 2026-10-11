package httpx

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Compress ---------------------------------------------------------------

// TestCompressSkipsTheWrongBodies is the important half of the compression
// tests: a middleware that gzips everything breaks byte ranges, corrupts
// already-compressed assets and wastes CPU on tiny responses.
func TestCompressSkipsTheWrongBodies(t *testing.T) {
	big := strings.Repeat("a", 4096)

	t.Run("range request is passed through untouched", func(t *testing.T) {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css")
			w.Header().Set("Content-Range", "bytes 0-9/100")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(big))
		}))
		req := httptest.NewRequest(http.MethodGet, "/f", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Range", "bytes=0-9")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q on a range response; gzip and ranges do not compose", got)
		}
	})

	t.Run("client did not ask for gzip", func(t *testing.T) {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte(big))
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f", nil))

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q without Accept-Encoding: gzip", got)
		}
	})

	t.Run("small response is not worth the framing", func(t *testing.T) {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte("body{}"))
		}))
		req := httptest.NewRequest(http.MethodGet, "/f", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q below the size floor", got)
		}
	})

	t.Run("already-compressed type is left alone", func(t *testing.T) {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "font/woff2")
			_, _ = w.Write([]byte(big))
		}))
		req := httptest.NewRequest(http.MethodGet, "/fonts/x.woff2", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q for a woff2 body; it is already compressed", got)
		}
	})

	t.Run("non-200 keeps its body unencoded", func(t *testing.T) {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotModified)
		}))
		req := httptest.NewRequest(http.MethodGet, "/f", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q on a 304", got)
		}
	})

	t.Run("HEAD has no body to compress", func(t *testing.T) {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte(big))
		}))
		req := httptest.NewRequest(http.MethodHead, "/f", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q on a HEAD response", got)
		}
	})
}

// TestCompressShrinksAndStaysReadable checks the positive path: the body really
// is gzipped, really is smaller, and really round-trips.
func TestCompressShrinksAndStaysReadable(t *testing.T) {
	body := strings.Repeat("@font-face{font-family:'Noto Sans SC'}\n", 400)

	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))

	req := httptest.NewRequest(http.MethodGet, "/yeciorez/teleport/fonts/fonts.css", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	// A shared cache must not hand a gzipped body to a client that cannot read
	// it, so the response has to vary on Accept-Encoding.
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q, must include Accept-Encoding", v)
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Error("Content-Length survived gzip; it no longer describes the body")
	}

	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if string(got) != body {
		t.Errorf("round-trip mismatch: got %d bytes, want %d", len(got), len(body))
	}
	if rec.Body.Len() >= len(body) {
		t.Errorf("gzipped body (%d) is not smaller than the original (%d)", rec.Body.Len(), len(body))
	}
}

// TestCompressSurvivesServeContent is the regression test for the ordering trap.
//
// Every static asset here is written by `http.ServeContent`, which sets
// `Content-Length` and flushes the header *before* the first body byte. A
// middleware that decides on the first `Write` is therefore too late: the client
// has already been told the uncompressed length, then receives gzip bytes, and
// the response is truncated from the client's point of view.
//
// This asserts the whole contract end to end — status, declared length,
// encoding header, and a body that actually decompresses to the original — so
// it fails loudly on that mistake rather than shipping a corrupted asset.
func TestCompressSurvivesServeContent(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefghij"), 20_000) // 200 KB

	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		// Exactly what the spa handler does for fonts/fonts.css.
		http.ServeContent(w, r, "fonts.css", time.Time{}, bytes.NewReader(body))
	}))

	req := httptest.NewRequest(http.MethodGet, "/yeciorez/teleport/fonts/fonts.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip (ServeContent sets a length before the first write)", got)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q survived gzip; it describes the unencoded body", cl)
	}

	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(body))
	}
	if rec.Body.Len() >= len(body) {
		t.Errorf("gzipped body (%d) is not smaller than the original (%d)", rec.Body.Len(), len(body))
	}
}

// TestCompressPreservesRangeThroughServeContent checks that the pass-through for
// ranged requests still produces a correct partial body.
func TestCompressPreservesRangeThroughServeContent(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 1000)

	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		http.ServeContent(w, r, "f.txt", time.Time{}, bytes.NewReader(body))
	}))

	req := httptest.NewRequest(http.MethodGet, "/f.txt", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=10-19")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q on a range response", got)
	}
	if got := rec.Body.String(); got != "0123456789" {
		t.Errorf("range body = %q, want %q", got, "0123456789")
	}
}
