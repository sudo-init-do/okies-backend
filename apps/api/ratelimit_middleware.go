package main

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// use a UNIQUE helper name so we don't clash with any other clientIP in this package
func remoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (app *App) rateLimit(limit int, window time.Duration, keyf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// If Redis isn't configured/available, skip limiting.
			if app.Redis == nil {
				next.ServeHTTP(w, r)
				return
			}

			key := "rl:" + r.URL.Path + ":" + keyf(r)
			pipe := app.Redis.TxPipeline()
			incr := pipe.Incr(r.Context(), key)
			pipe.Expire(r.Context(), key, window)

			if _, err := pipe.Exec(r.Context()); err != nil {
				httpError(w, http.StatusInternalServerError, "rate_limit_error")
				return
			}
			if incr.Val() > int64(limit) {
				httpError(w, http.StatusTooManyRequests, "rate_limited")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (app *App) RateLimitIP(limit int, window time.Duration) func(http.Handler) http.Handler {
	return app.rateLimit(limit, window, func(r *http.Request) string { return "ip:" + remoteIP(r) })
}

func (app *App) RateLimitUser(limit int, window time.Duration) func(http.Handler) http.Handler {
	return app.rateLimit(limit, window, func(r *http.Request) string {
		if uid, ok := getUserID(r); ok && uid != "" {
			return "uid:" + uid
		}
		return "ip:" + remoteIP(r)
	})
}

// keep redis import from being trimmed in some builds
var _ = redis.Nil
