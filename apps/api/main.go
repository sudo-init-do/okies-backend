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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	mydb "github.com/sudo-init-do/okies-backend/pkg/db"
)

type App struct {
	DB        *pgxpool.Pool
	JWTSecret []byte
	Redis     *redis.Client
}

type UserDTO struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	Username    *string   `json:"username,omitempty"`
	DisplayName *string   `json:"displayName,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	port := getenv("PORT", "8081")

	// graceful shutdown context
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// DB pool
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

	app := &App{
		DB:        pool,
		JWTSecret: []byte(getenv("JWT_SECRET", "dev_change_me")),
		Redis:     rdb,
	}

	// Router
	r := chi.NewRouter()

	// --- Health ---
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(c); err != nil {
			http.Error(w, "db not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ready"))
	})

	// --- Public webhooks ---
	// Flutterwave will call this; verify with FLW_WEBHOOK_HASH in handler
	r.Post("/v1/webhooks/flutterwave", app.FlutterwaveWebhook)

	// --- Public auth (rate-limited by IP) ---
	r.With(app.RateLimitIP(10, time.Minute)).Post("/v1/auth/signup", app.Signup)
	r.With(app.RateLimitIP(20, time.Minute)).Post("/v1/auth/login", app.Login)
	r.With(app.RateLimitIP(30, time.Minute)).Post("/v1/auth/refresh", app.Refresh)

	// --- Protected ---
	r.Group(func(pr chi.Router) {
		pr.Use(app.AuthMiddleware)

		// self
		pr.Get("/v1/auth/me", app.Me)
		pr.Get("/v1/auth/whoami", app.WhoAmI)

		// wallet
		pr.Get("/v1/wallet", app.GetWallet)
		pr.Get("/v1/wallet/transactions", app.ListWalletTransactions)
		pr.Get("/v1/wallet/withdrawals", app.ListMyWithdrawals)

		// payout destinations (user adds bank account for withdrawals)
		pr.Post("/v1/payout-destinations", app.CreatePayoutDestination)

		// gifting (rate-limited per user)
		pr.With(app.RateLimitUser(60, time.Minute)).Post("/v1/gifts", app.CreateGift)

		// users
		pr.Get("/v1/users/search", app.SearchUsers)

		// withdrawals (user submit)
		pr.Post("/v1/wallet/withdrawals", app.CreateWithdrawal)

		// admin actions
		pr.Group(func(ad chi.Router) {
			ad.Use(app.RequireAdmin)
			ad.Post("/v1/admin/topups", app.AdminTopup)
			ad.Post("/v1/admin/withdrawals/{id}/approve", app.AdminApproveWithdrawal)
			ad.Post("/v1/admin/withdrawals/{id}/reject", app.AdminRejectWithdrawal)
		})
	})

	// dev sanity: list users
	r.Get("/v1/users", func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id, email, username, display_name, created_at
			FROM users
			ORDER BY created_at DESC
			LIMIT 50;`)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var out []UserDTO
		for rows.Next() {
			var u UserDTO
			if err := rows.Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.CreatedAt); err != nil {
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
