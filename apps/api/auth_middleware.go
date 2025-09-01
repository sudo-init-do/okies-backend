package main

import (
	"context"
	"net/http"
	"strings"

	a "github.com/sudo-init-do/okies-backend/pkg/auth"
)

type ctxKey string

const (
	ctxUserID   ctxKey = "userID"
	ctxUserRole ctxKey = "userRole"
)

type AccessClaims = a.AccessClaims

func (app *App) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			httpError(w, http.StatusUnauthorized, "missing_bearer_token")
			return
		}
		tokenStr := strings.TrimPrefix(authz, "Bearer ")
		claims, err := a.ParseAccess(app.JWTSecret, tokenStr)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "invalid_token")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID, claims.Subject)
		ctx = context.WithValue(ctx, ctxUserRole, claims.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (app *App) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, _ := getUserRole(r)
		if role != "admin" {
			httpError(w, http.StatusForbidden, "admin_only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func getUserID(r *http.Request) (string, bool) {
	v := r.Context().Value(ctxUserID)
	if v == nil { return "", false }
	s, ok := v.(string)
	return s, ok
}
func getUserRole(r *http.Request) (string, bool) {
	v := r.Context().Value(ctxUserRole)
	if v == nil { return "", false }
	s, ok := v.(string)
	return s, ok
}
