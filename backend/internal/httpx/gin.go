package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GinMiddleware adapts one of this package's net/http middlewares so it can be
// registered on a Gin route.
//
// Why the transport middleware stays net/http, and why only this thin adapter is
// needed to use it from Gin, is a decision with a record:
// .ai/decisions/2026-10-16-p3-gin-integration.md. The short version is that the
// compression writer and the CORS preflight both depend on wrapping the *raw*
// ResponseWriter — and on CORS running before routing — which Gin's own writer
// does not give us. Re-earning those two behaviours inside Gin would buy nothing
// observable and would risk two documented silent failure modes. So Gin owns
// routing and dispatch — the route table, method matching, params, the 404
// fallback — and the transport chain keeps wrapping the engine.
//
// What this adapter has to get right is the *context handoff*. These middlewares
// publish what they learned (the request id, the session, the agent principal)
// through the request context, and everything downstream reads it that way:
// PrincipalFrom(c.Request.Context()), RequestIDFrom(...), the server-rendered
// hand-off page. A Gin handler receives *gin.Context instead, so the adapter must
// carry the middleware's enriched request back onto the Gin context before the
// chain continues — otherwise a route would be authenticated and then see no
// principal.
//
// The refusal path needs the same care in the other direction: a middleware that
// rejects a request writes the response itself and never calls next. Gin must
// then Abort, or the handler would run anyway and write a second response.
func GinMiddleware(mw Middleware) gin.HandlerFunc {
	return func(c *gin.Context) {
		proceeded := false
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			proceeded = true
			c.Request = r
			c.Next()
		})

		// c.Writer is a gin.ResponseWriter, which satisfies http.ResponseWriter,
		// so the middleware's own writes (including the JSON error envelope) go
		// out unchanged.
		mw(next).ServeHTTP(c.Writer, c.Request)

		if !proceeded {
			c.Abort()
		}
	}
}
