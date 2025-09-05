package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	mydb "github.com/sudo-init-do/okies-backend/pkg/db"
)

type App struct {
	DB          *pgxpool.Pool
	JWTSecret   []byte
	Redis       *redis.Client
	Flutterwave FlutterwaveClient
}

type UserDTO struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	Username    *string   `json:"username,omitempty"`
	DisplayName *string   `json:"displayName,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// custom response writer to capture status codes
type logResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *logResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.SetGlobalLevel(zerolog.DebugLevel) // 👈 show all logs
	port := getenv("PORT", "8081")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// DB
	pool := mydb.MustOpenPool(ctx)
	defer pool.Close()

	// Redis (optional)
	var rdb *redis.Client
	rc := redis.NewClient(&redis.Options{
		Addr: getenv("REDIS_ADDR", "localhost:6379"),
	})
	if err := rc.Ping(ctx).Err(); err != nil {
		log.Warn().Err(err).Msg("redis not reachable; rate limiting disabled")
	} else {
		rdb = rc
		defer rdb.Close()
	}

	// Flutterwave client
	flw, err := NewFlutterwaveClient(
		getenv("FLW_BASE_URL", "https://api.flutterwave.com"),
		getenv("FLW_SEC_KEY", ""),
		getenv("FLW_ENC_KEY", ""),
	)
	if err != nil {
		log.Warn().Err(err).Msg("flutterwave not configured; payouts will be dry-run until set")
	}

	app := &App{
		DB:          pool,
		JWTSecret:   []byte(getenv("JWT_SECRET", "dev_change_me")),
		Redis:       rdb,
		Flutterwave: flw,
	}

	r := chi.NewRouter()
	r.Use(cors.AllowAll().Handler)

	// 🔎 Logging middleware
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			lrw := &logResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			// panic recovery
			defer func() {
				if rec := recover(); rec != nil {
					log.Error().
						Interface("panic", rec).
						Str("url", req.URL.String()).
						Msg("panic recovered")
					http.Error(lrw, "internal server error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(lrw, req)
			duration := time.Since(start)

			if lrw.statusCode >= 400 {
				log.Error().
					Str("method", req.Method).
					Str("url", req.URL.String()).
					Int("status", lrw.statusCode).
					Dur("duration", duration).
					Msg("request failed")
			} else {
				log.Debug().
					Str("method", req.Method).
					Str("url", req.URL.String()).
					Int("status", lrw.statusCode).
					Dur("duration", duration).
					Msg("request completed")
			}
		})
	})

	// Health
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(c); err != nil {
			log.Error().Err(err).Msg("db ping failed")
			http.Error(w, "db not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ready")) })

	// Public webhooks
	r.Post("/v1/webhooks/flutterwave", app.FlutterwaveWebhook)

	// Public auth
	r.With(app.RateLimitIP(10, time.Minute)).Post("/v1/auth/signup", app.Signup)
	r.With(app.RateLimitIP(20, time.Minute)).Post("/v1/auth/login", app.Login)
	r.With(app.RateLimitIP(30, time.Minute)).Post("/v1/auth/refresh", app.Refresh)

	// Protected
	r.Group(func(pr chi.Router) {
		pr.Use(app.AuthMiddleware)

		// self
		pr.Get("/v1/auth/me", app.Me)
		pr.Get("/v1/auth/whoami", app.WhoAmI)

		// wallet
		pr.Get("/v1/wallet", app.GetWallet)
		pr.Get("/v1/wallet/transactions", app.ListWalletTransactions)
		pr.Get("/v1/wallet/withdrawals", app.ListMyWithdrawals)

		// gifting
		pr.With(app.RateLimitUser(60, time.Minute)).Post("/v1/gifts", app.CreateGift)

		// users
		pr.Get("/v1/users/search", app.SearchUsers)

		// payout destinations
		pr.Get("/v1/payout-destinations", app.ListPayoutDestinations)
		pr.Post("/v1/payout-destinations", app.CreatePayoutDestination)
		pr.Delete("/v1/payout-destinations/{id}", app.DeletePayoutDestination)

		// withdrawals
		pr.Post("/v1/withdrawals", app.CreateWithdrawal)

		// admin
		pr.Group(func(ad chi.Router) {
			ad.Use(app.RequireAdmin)
			ad.Post("/v1/admin/topups", app.AdminTopup)
			ad.Post("/v1/admin/withdrawals/{id}/approve", app.AdminApproveWithdrawal)
			ad.Post("/v1/admin/withdrawals/{id}/reject", app.AdminRejectWithdrawal)
		})
	})

	// dev: quick users list
	r.Get("/v1/users", func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id, email, username, display_name, created_at
			FROM users
			ORDER BY created_at DESC
			LIMIT 50`)
		if err != nil {
			log.Error().Err(err).Msg("failed to query users")
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var out []UserDTO
		for rows.Next() {
			var u UserDTO
			if err := rows.Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.CreatedAt); err != nil {
				log.Error().Err(err).Msg("failed to scan user row")
				http.Error(w, "scan failed", http.StatusInternalServerError)
				return
			}
			out = append(out, u)
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	})

	addr := fmt.Sprintf(":%s", port)
	log.Info().Msgf("API running on %s", addr)

	srv := &http.Server{Addr: addr, Handler: r}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Info().Msg("server shutdown complete")
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
