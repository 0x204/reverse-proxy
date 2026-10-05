package main

import (
	"net/http"
	"strings"
)

// corsPolicy lets the proxy answer CORS for the backend. The zero value leaves
// CORS entirely to the backend.
type corsPolicy struct {
	anyOrigin bool
	origins   []string
}

// parseCORS reads the allow_origins setting: "*" or a comma-separated list.
func parseCORS(allowOrigins string) corsPolicy {
	var c corsPolicy
	for _, origin := range strings.Split(allowOrigins, ",") {
		switch origin = strings.TrimSpace(origin); origin {
		case "":
		case "*":
			c.anyOrigin = true
		default:
			c.origins = append(c.origins, origin)
		}
	}
	return c
}

func (c corsPolicy) enabled() bool {
	return c.anyOrigin || len(c.origins) > 0
}

// allow sets Access-Control-Allow-Origin in h if origin is allowed, replacing
// whatever the backend sent.
func (c corsPolicy) allow(h http.Header, origin string) bool {
	if c.anyOrigin {
		h.Set("Access-Control-Allow-Origin", "*")
		return true
	}
	if len(c.origins) == 0 {
		return false
	}
	// The response depends on Origin even when it does not match, so a shared
	// cache must not serve it to a different origin.
	h.Add("Vary", "Origin")
	for _, o := range c.origins {
		if strings.EqualFold(o, origin) {
			h.Set("Access-Control-Allow-Origin", origin)
			return true
		}
	}
	h.Del("Access-Control-Allow-Origin")
	return false
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

func (c corsPolicy) preflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if c.allow(h, r.Header.Get("Origin")) {
		h.Set("Access-Control-Allow-Methods", r.Header.Get("Access-Control-Request-Method"))
		if headers := r.Header.Get("Access-Control-Request-Headers"); headers != "" {
			h.Set("Access-Control-Allow-Headers", headers)
		}
		h.Set("Access-Control-Max-Age", "3600")
	}
	w.WriteHeader(http.StatusNoContent)
}
