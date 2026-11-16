package auth

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
)

type ctxKey struct{}

// WithViewer returns ctx carrying v (used by the middleware; handy in tests and background jobs).
func WithViewer(ctx context.Context, v Viewer) context.Context {
	return context.WithValue(ctx, ctxKey{}, v)
}

// ViewerFrom returns the request's viewer; a guest (empty UserID) when none was resolved.
func ViewerFrom(ctx context.Context) Viewer {
	v, _ := ctx.Value(ctxKey{}).(Viewer)
	return v
}

// Middleware resolves the viewer from "Authorization: Bearer <token>" or the sewain_session cookie and stores
// it in the request context. Invalid or expired credentials make the request a guest request (handlers decide
// with RequireUser/RequireAdmin). Cookie-authenticated state-changing requests must carry an Origin (or
// Referer) from AllowedOrigins, otherwise they are rejected with 403 (CSRF, D6). Credential-less
// state-changing requests that carry a foreign Origin are rejected too (login CSRF). Bearer requests are
// exempt because browsers never attach that header cross-site on their own.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, fromCookie := bearerToken(r), false
		if token == "" {
			if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
				token, fromCookie = c.Value, true
			}
		}
		// Cookie-authenticated mutations need an allowed Origin/Referer. Cookieless, non-bearer mutations (the
		// OTP login endpoints) are refused when a browser names a foreign origin, which stops login CSRF;
		// clients that send no Origin at all (apps, curl) are unaffected.
		checkOrigin := fromCookie || (token == "" && r.Header.Get("Origin") != "")
		if checkOrigin && !safeMethod(r.Method) && !s.OriginAllowed(r) {
			WriteError(w, userErr(http.StatusForbidden, MsgBadOrigin))
			return
		}
		var v Viewer
		if token != "" {
			viewer, touched, err := s.Resolve(r.Context(), token)
			switch {
			case err == nil:
				v = viewer
				// Per-user rate limits and access logs read the user from the platform request info.
				middleware.SetUserID(r.Context(), v.UserID)
				if touched && fromCookie {
					s.SetSessionCookie(w, token)
				}
			case err == ErrNoSession:
				if fromCookie {
					s.ClearSessionCookie(w)
				}
			default:
				log.Printf("auth: resolve session: %v", err)
				WriteError(w, err)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(WithViewer(r.Context(), v)))
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions || m == http.MethodTrace
}

// OriginAllowed reports whether the request's Origin (or, without Origin, its Referer) is one of the
// configured AllowedOrigins. With no AllowedOrigins configured (development), the request's own host is
// accepted. "Origin: null" is never allowed.
func (s *Service) OriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "null" {
		// Opaque origin (sandboxed frame, cross-origin redirect): the browser hides the initiator on purpose.
		return false
	}
	if origin == "" {
		ref := r.Header.Get("Referer")
		if ref == "" {
			return false
		}
		u, err := url.Parse(ref)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return false
		}
		origin = u.Scheme + "://" + u.Host
	}
	origin = strings.TrimRight(origin, "/")
	if len(s.Config.AllowedOrigins) == 0 {
		u, err := url.Parse(origin)
		return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
	}
	for _, o := range s.Config.AllowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// SetSessionCookie writes the session cookie (HttpOnly, SameSite=Lax, Path=/, Secure unless disabled).
func (s *Service) SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/", MaxAge: int(SessionTTL.Seconds()),
		HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie deletes the session cookie.
func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// ClientIP returns the client IP. When the platform chain ran (middleware.RealIP), its answer is used: it
// only believes proxy headers from TRUSTED_PROXY_CIDRS. Otherwise the configured Config.ClientIPHeader is
// read (for X-Forwarded-For the last hop, the one our own proxy appended; earlier hops are written by the
// client), and finally the connection's remote address.
func (s *Service) ClientIP(r *http.Request) string {
	if ip := middleware.ClientIPFrom(r.Context()); ip != "" {
		return ip
	}
	if h := s.Config.ClientIPHeader; h != "" {
		hops := strings.Split(strings.Join(r.Header.Values(h), ","), ",")
		v := hops[0]
		if strings.EqualFold(h, "X-Forwarded-For") {
			v = hops[len(hops)-1]
		}
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Meta extracts RequestMeta (IP, User-Agent) from r.
func (s *Service) Meta(r *http.Request) RequestMeta {
	return RequestMeta{IP: s.ClientIP(r), UserAgent: r.UserAgent()}
}

// RequireUser rejects guests with 401.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ViewerFrom(r.Context()).IsGuest() {
			WriteError(w, userErr(http.StatusUnauthorized, MsgLoginRequired))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin rejects guests with 401 and non-admins with 403.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := ViewerFrom(r.Context())
		if v.IsGuest() {
			WriteError(w, userErr(http.StatusUnauthorized, MsgLoginRequired))
			return
		}
		if !v.IsAdmin() {
			WriteError(w, userErr(http.StatusForbidden, MsgAdminOnly))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUserFunc and RequireAdminFunc are HandlerFunc conveniences.
func RequireUserFunc(f http.HandlerFunc) http.Handler  { return RequireUser(f) }
func RequireAdminFunc(f http.HandlerFunc) http.Handler { return RequireAdmin(f) }

// CheckAdmin is the non-HTTP form of RequireAdmin for service code (e.g. POST /api/v1/actions/{name}).
func CheckAdmin(v Viewer) error {
	if v.IsGuest() {
		return userErr(http.StatusUnauthorized, MsgLoginRequired)
	}
	if !v.IsAdmin() {
		return userErr(http.StatusForbidden, MsgAdminOnly)
	}
	return nil
}
