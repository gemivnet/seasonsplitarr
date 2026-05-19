// Package logging provides small helpers around the stdlib log package so
// every seasonsplitarr component emits docker-friendly, prefix-tagged lines
// to stderr. Output is intentionally verbose — when something misbehaves in
// the *arr stack the user needs the full trail in `docker logs`.
package logging

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// L is a tiny tagged logger. Get one per package with New("torznab").
type L struct {
	tag string
}

func New(tag string) *L { return &L{tag: tag} }

func (l *L) Printf(format string, args ...any) {
	log.Printf("["+l.tag+"] "+format, args...)
}

func (l *L) Info(format string, args ...any)  { l.Printf("INFO  "+format, args...) }
func (l *L) Warn(format string, args ...any)  { l.Printf("WARN  "+format, args...) }
func (l *L) Error(format string, args ...any) { l.Printf("ERROR "+format, args...) }
func (l *L) Debug(format string, args ...any) { l.Printf("DEBUG "+format, args...) }

// Redact returns a short fingerprint of a secret suitable for logs:
// the first 4 chars then "…" then the last 2 chars, or "<empty>" / "<short>".
func Redact(s string) string {
	if s == "" {
		return "<empty>"
	}
	if len(s) < 8 {
		return "<short:" + strings.Repeat("*", len(s)) + ">"
	}
	return s[:4] + "…" + s[len(s)-2:]
}

// RequestLog wraps an http.Handler and logs every request after it completes
// with method, path, status, response size, duration, and remote.
func RequestLog(tag string, h http.Handler) http.Handler {
	l := New(tag)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &recorder{ResponseWriter: w, status: 200}
		// Log the inbound request before serving so a panicking handler still
		// shows up in the logs.
		l.Info("--> %s %s from %s ua=%q", r.Method, sanitizePath(r.URL.RequestURI()), r.RemoteAddr, r.UserAgent())
		h.ServeHTTP(rw, r)
		l.Info("<-- %s %s %d (%d bytes, %s)", r.Method, sanitizePath(r.URL.RequestURI()), rw.status, rw.n, time.Since(start))
	})
}

// sanitizePath strips the apikey query param value from URLs before logging.
func sanitizePath(uri string) string {
	const k = "apikey="
	i := strings.Index(uri, k)
	if i < 0 {
		return uri
	}
	rest := uri[i+len(k):]
	end := strings.IndexAny(rest, "&")
	if end < 0 {
		return uri[:i+len(k)] + "***"
	}
	return uri[:i+len(k)] + "***" + rest[end:]
}

type recorder struct {
	http.ResponseWriter
	status int
	n      int
}

func (r *recorder) WriteHeader(s int) {
	r.status = s
	r.ResponseWriter.WriteHeader(s)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.n += n
	return n, err
}
