package configcenter

import (
	"context"
	"net/http"
)

func contextWithPrincipal(r *http.Request, p *principal) context.Context {
	return context.WithValue(r.Context(), principalContextKey{}, p)
}

func principalFrom(r *http.Request) *principal {
	if p, ok := r.Context().Value(principalContextKey{}).(*principal); ok {
		return p
	}
	return &principal{}
}
