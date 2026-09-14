package admin

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// bearerPrefix is the scheme the Authorization header uses. The header is also
// accepted bare under X-Admin-Token, because the first thing anybody does with
// an admin API is curl it.
const bearerPrefix = "Bearer "

// tokenHeader is the alternative to Authorization, for callers that would
// otherwise have to remember the scheme name.
const tokenHeader = "X-Admin-Token"

// maxBodyBytes bounds a request body. No endpoint reads one in v0.1.0 — every
// route is a GET — so this exists to stop an unauthenticated caller from
// streaming megabytes at the port and having them buffered by anything that
// later does read one.
const maxBodyBytes = 1 << 20

// errorResponse is what a refusal says. It never echoes what was presented:
// repeating a supplied token back into a response body puts it into the
// caller's logs, into a proxy's access log, and into a screenshot.
type errorResponse struct {
	Error string `json:"error"`
}

// protected wraps a handler in the admin token check (ADR-0023).
//
// An empty configured token leaves the handler open. That is only reachable on
// a loopback bind, because config validation refuses a non-loopback bind_addr
// with no admin.token — so the open case is the deliberate single-host default,
// not an accident. The guard is in configuration rather than here because a
// process that would serve statistics to the world should not start at all,
// rather than start and refuse every request.
func (s *Server) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next.ServeHTTP(w, r)
			return
		}

		if !s.authorized(r) {
			// The scheme is named so a caller knows how to retry; nothing else
			// is disclosed, including whether a token was presented at all.
			w.Header().Set("WWW-Authenticate", `Bearer realm="atlascache admin"`)
			s.writeJSON(w, http.StatusUnauthorized, errorResponse{
				Error: "admin token required: send it as `Authorization: Bearer <admin.token>` or `" +
					tokenHeader + ": <admin.token>`",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

// authorized reports whether the request carries the admin token.
//
// The comparison is constant-time. The window it closes is narrow over a
// network, but the cost of closing it is one function call, and a timing oracle
// on an administrative credential is not a thing to leave open for that price.
func (s *Server) authorized(r *http.Request) bool {
	presented := r.Header.Get(tokenHeader)
	if presented == "" {
		authorization := r.Header.Get("Authorization")
		if len(authorization) > len(bearerPrefix) &&
			strings.EqualFold(authorization[:len(bearerPrefix)], bearerPrefix) {
			presented = authorization[len(bearerPrefix):]
		}
	}

	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

// recoverPanics turns a handler panic into a 500 for that one request.
//
// The requirement is not politeness. Without it the panic unwinds out of the
// serving goroutine and kills the process, which would make the admin port —
// the one endpoint that is reachable without a credential — a way to take the
// cache down. A panic is still a bug, so it is logged with its stack at error
// level rather than swallowed.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			// net/http panics with this to abandon a response on purpose; it is
			// not a bug and must be left to the server to handle.
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}

			s.log.Error().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Interface("panic", recovered).
				Bytes("stack", debug.Stack()).
				Msg("admin handler panicked")

			// Writing after the handler has already written its own header is a
			// no-op with a logged complaint from net/http, which is the right
			// outcome: the request is broken either way and the process is not.
			s.writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		}()

		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// logRequests records one line per request: what was asked, what was answered,
// and how long it took.
//
// It records the path and never the query string or any header. That is the
// rule that keeps credentials out of the log (ADR-0020's requirement for the
// client token, applied to this one): a token arrives in a header, and a header
// that is never read cannot be logged by accident.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		s.log.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", recorder.status).
			Int("bytes", recorder.written).
			Dur("took", time.Since(started)).
			Str("remote", r.RemoteAddr).
			Msg("admin request")
	})
}

// statusRecorder remembers what the handler answered, so the log line reports
// the response rather than assuming it.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
	wrote   bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// writeJSON renders a response body.
//
// Encoding into a buffer first is what makes the status code honest: encoding
// straight into the ResponseWriter would send 200 and then fail halfway through
// a body, leaving the caller with a truncated document under a success code.
//
// HTML escaping is off. These responses are application/json and are never
// interpolated into a page, and the escaping costs twice: an operator reading
// /config sees \u003credacted\u003e instead of <redacted>, and a scan of the
// body for a leaked secret can be defeated by a secret containing a character
// the encoder rewrote.
func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(payload); err != nil {
		s.log.Error().Err(err).Msg("admin response could not be encoded")
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	body := buf.Bytes()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		s.log.Debug().Err(err).Msg("admin response write failed")
	}
}
