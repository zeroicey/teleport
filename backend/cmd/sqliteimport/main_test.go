package main

import (
	"strings"
	"testing"
)

// TestDescribeDSNRedactsEveryForm is a guard on a function whose whole purpose is to
// keep a password out of a log. The first version handled only the URL form, so a
// keyword/value DSN ("host=… password=…") was printed unchanged — contradicting the
// function's own comment.
func TestDescribeDSNRedactsEveryForm(t *testing.T) {
	const secret = "hunter2"
	for _, tc := range []struct {
		name string
		dsn  string
	}{
		{"url with userinfo", "postgres://teleport:" + secret + "@127.0.0.1:5432/teleport?sslmode=disable"},
		{"url whose password contains @", "postgres://teleport:aa" + secret + "@bb@127.0.0.1:5432/teleport"},
		{"keyword/value", "host=127.0.0.1 user=teleport password=" + secret + " dbname=teleport"},
		{"keyword/value with passfile", "host=127.0.0.1 password=" + secret + " passfile=/root/.pgpass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeDSN(tc.dsn)
			if strings.Contains(got, secret) {
				t.Errorf("describeDSN leaked the password: %q", got)
			}
			if got == "" {
				t.Error("describeDSN returned an empty string; an operator needs to see which target it is")
			}
		})
	}

	// A DSN with no credentials must still be recognisable, not blanked.
	if got := describeDSN("postgres://127.0.0.1:5432/teleport"); !strings.Contains(got, "teleport") {
		t.Errorf("describeDSN dropped the database name: %q", got)
	}
}
