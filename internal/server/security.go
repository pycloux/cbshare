package server

// Browser-facing protections. Requests from other web sites (CSRF) are
// stopped by several independent layers, so that one weakness is not
// enough:
//
//  1. Cookies are SameSite=Strict: browsers do not attach them to
//     requests started by another site.
//  2. sameOrigin rejects state-changing requests whose Origin (or
//     Sec-Fetch-Site) header shows they come from another site.
//  3. Signed-in requests must carry the session's secret token in the
//     X-CSRF-Token header, which another site cannot read or guess.
//  4. JSON endpoints require Content-Type: application/json, which plain
//     HTML forms cannot send.
//
// Cross-site scripting is contained by the Content Security Policy below,
// and by the front end only ever inserting messages as text.

import (
	"mime"
	"net/http"
	"strings"

	"github.com/pycloux/cpsync/internal/crypto"
)

// contentSecurityPolicy allows only the embedded front end: no inline
// scripts or styles, no third-party content, no framing.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; manifest-src 'self'; form-action 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// securityHeaders adds hardening headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// One day, without subdomains: other services on the same host
		// name are not forced to HTTPS for long. Browsers ignore HSTS for
		// IP addresses and untrusted certificates anyway.
		h.Set("Strict-Transport-Security", "max-age=86400")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-cache") // revalidate so upgrades take effect
		}
		next.ServeHTTP(w, r)
	})
}

// unsafeMethod reports whether a request may change state.
func unsafeMethod(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// sameOrigin rejects state-changing requests sent by other web sites.
// Browsers always send Origin (or at least Sec-Fetch-Site) on such
// requests; command-line clients send neither and are allowed.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unsafeMethod(r) {
			if origin := r.Header.Get("Origin"); origin != "" {
				if origin != "https://"+r.Host {
					writeError(w, http.StatusForbidden, "cross_origin", "Requests from other web sites are not allowed.")
					return
				}
			} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeError(w, http.StatusForbidden, "cross_origin", "Requests from other web sites are not allowed.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// checkCSRF requires the session's anti-CSRF token on state-changing
// requests, in the X-CSRF-Token header.
func checkCSRF(w http.ResponseWriter, r *http.Request, token string) bool {
	if !unsafeMethod(r) || crypto.Equal(r.Header.Get("X-CSRF-Token"), token) {
		return true
	}
	writeError(w, http.StatusForbidden, "csrf", "This page is out of date. Reload it and try again.")
	return false
}

// isJSON reports whether the request declares a JSON body. Requiring it
// means cross-site HTML forms cannot reach the JSON endpoints.
func isJSON(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}
