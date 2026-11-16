package httpapi

import (
	"net/http"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/service"
)

// ViewerResolver turns a request into the acting viewer: nil (guest) or {UserID, Role}. An error means the request
// could not be checked (database down) and is answered with 500. The auth module provides an OTP + cookie
// implementation; DemoTokens is the DEMO_MODE bearer token implementation.
type ViewerResolver interface {
	Resolve(r *http.Request) (*service.Viewer, error)
}

// ViewerResolverFunc adapts a function.
type ViewerResolverFunc func(r *http.Request) (*service.Viewer, error)

func (f ViewerResolverFunc) Resolve(r *http.Request) (*service.Viewer, error) { return f(r) }

// Chain tries resolvers in order and returns the first viewer found (e.g. session cookie, then demo bearer token).
func Chain(resolvers ...ViewerResolver) ViewerResolver {
	return ViewerResolverFunc(func(r *http.Request) (*service.Viewer, error) {
		for _, res := range resolvers {
			v, err := res.Resolve(r)
			if err != nil || v != nil {
				return v, err
			}
		}
		return nil, nil
	})
}

// BearerToken reads "Authorization: Bearer <token>" or, for WebSocket/img requests that cannot send headers,
// the "token" query parameter.
func BearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return r.URL.Query().Get("token")
}

// DemoTokens resolves demo bearer tokens (POST /api/v1/auth/demo-login). Only the SHA-256 of a token is stored.
type DemoTokens struct{ Svc *service.Service }

func (d DemoTokens) Resolve(r *http.Request) (*service.Viewer, error) {
	token := BearerToken(r)
	if token == "" {
		return nil, nil
	}
	uid, err := d.Svc.DemoSessionUser(r.Context(), token)
	if err != nil || uid == "" {
		return nil, err
	}
	u, err := d.Svc.GetUserAuth(r.Context(), uid)
	if err != nil || u == nil {
		return nil, err // user gone (e.g. after a demo reset): guest
	}
	return &service.Viewer{UserID: u.ID, Role: u.Role}, nil
}
