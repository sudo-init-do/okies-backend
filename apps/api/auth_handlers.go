package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	a "github.com/sudo-init-do/okies-backend/pkg/auth"
)

type signupReq struct {
	Email       string  `json:"email"`
	Password    string  `json:"password"`
	Username    *string `json:"username,omitempty"`
	DisplayName *string `json:"displayName,omitempty"`
}
type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}
type authResp struct {
	Tokens a.TokenPair `json:"tokens"`
	User   UserDTO     `json:"user"`
}

func (app *App) Signup(w http.ResponseWriter, r *http.Request) {
	var body signupReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	body.Email = strings.ToLower(strings.TrimSpace(body.Email))
	if body.Email == "" || body.Password == "" {
		httpError(w, http.StatusBadRequest, "email_and_password_required")
		return
	}

	var exists bool
	if err := app.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE email=$1)`, body.Email).Scan(&exists); err != nil {
		log.Error().Err(err).Msg("db EXISTS(users) failed")
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	if exists {
		httpError(w, http.StatusConflict, "email_in_use")
		return
	}

	hash, err := a.HashPassword(body.Password)
	if err != nil {
		log.Error().Err(err).Msg("argon2 hash error")
		httpError(w, http.StatusInternalServerError, "hash_error")
		return
	}

	var id string
	err = app.DB.QueryRow(r.Context(), `
		INSERT INTO users (email, password_hash, role, username, display_name)
		VALUES ($1,$2,'user',$3,$4)
		RETURNING id
	`, body.Email, hash, body.Username, body.DisplayName).Scan(&id)
	if err != nil {
		log.Error().Err(err).Msg("insert user failed")
		httpError(w, http.StatusInternalServerError, "insert_user_error")
		return
	}
	if _, err := app.DB.Exec(r.Context(), `INSERT INTO wallets (user_id, balance) VALUES ($1, 0) ON CONFLICT DO NOTHING`, id); err != nil {
		log.Error().Err(err).Str("user_id", id).Msg("insert wallet failed")
	}

	resp, err := app.issueTokens(r, id, "user")
	if err != nil {
		log.Error().Err(err).Str("user_id", id).Msg("issueTokens failed (signup)")
		httpError(w, http.StatusInternalServerError, "token_issue_error")
		return
	}

	writeJSON(w, http.StatusCreated, authResp{Tokens: resp, User: app.loadUser(r, id)})
}

func (app *App) Login(w http.ResponseWriter, r *http.Request) {
	var body loginReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))

	var id, hash, role string
	err := app.DB.QueryRow(r.Context(),
		`SELECT id, password_hash, role FROM users WHERE email=$1`, email).
		Scan(&id, &hash, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	if err != nil {
		log.Error().Err(err).Str("email", email).Msg("select user on login failed")
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	ok, err := a.CheckPassword(body.Password, hash)
	if err != nil || !ok {
		httpError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}

	tokens, err := app.issueTokens(r, id, role)
	if err != nil {
		log.Error().Err(err).Str("user_id", id).Msg("issueTokens failed (login)")
		httpError(w, http.StatusInternalServerError, "token_issue_error")
		return
	}
	writeJSON(w, http.StatusOK, authResp{Tokens: tokens, User: app.loadUser(r, id)})
}

func (app *App) Refresh(w http.ResponseWriter, r *http.Request) {
	var body struct{ RefreshToken string `json:"refreshToken"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken == "" {
		httpError(w, http.StatusBadRequest, "invalid_json")
		return
	}

	token, err := jwt.ParseWithClaims(body.RefreshToken, &jwt.RegisteredClaims{}, func(t *jwt.Token) (interface{}, error) {
		return app.JWTSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		httpError(w, http.StatusUnauthorized, "invalid_refresh")
		return
	}
	claims := token.Claims.(*jwt.RegisteredClaims)
	userID, jti := claims.Subject, claims.ID

	var role string
	var revoked *time.Time
	var expires time.Time
	err = app.DB.QueryRow(r.Context(), `
		SELECT u.role, rt.revoked_at, rt.expires_at
		FROM refresh_tokens rt
		JOIN users u ON u.id = rt.user_id
		WHERE rt.user_id = $1 AND rt.jti = $2
	`, userID, jti).Scan(&role, &revoked, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (revoked != nil) || time.Now().After(expires) {
		httpError(w, http.StatusUnauthorized, "refresh_not_valid")
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("select refresh_token failed")
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	if _, err := app.DB.Exec(r.Context(), `UPDATE refresh_tokens SET revoked_at = now() WHERE jti = $1`, jti); err != nil {
		log.Error().Err(err).Str("jti", jti).Msg("revoke old refresh failed")
	}

	tokens, err := app.issueTokens(r, userID, role)
	if err != nil {
		log.Error().Err(err).Str("user_id", userID).Msg("issueTokens failed (refresh)")
		httpError(w, http.StatusInternalServerError, "token_issue_error")
		return
	}

	writeJSON(w, http.StatusOK, authResp{Tokens: tokens, User: app.loadUser(r, userID)})
}

func (app *App) Me(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": app.loadUser(r, uid)})
}

// ---- helpers ----

func (app *App) issueTokens(r *http.Request, userID, role string) (a.TokenPair, error) {
	accessTTL := minutesFromEnv("ACCESS_TOKEN_TTL_MIN", 15)
	refreshTTL := daysFromEnv("REFRESH_TOKEN_TTL_DAYS", 30)

	access, err := a.GenerateAccess(app.JWTSecret, userID, role, accessTTL)
	if err != nil {
		return a.TokenPair{}, err
	}

	jti := uuid.NewString()
	refresh, err := a.GenerateRefresh(app.JWTSecret, userID, jti, refreshTTL)
	if err != nil {
		return a.TokenPair{}, err
	}

	ua, ip := r.UserAgent(), clientIP(r)
	expiresAt := time.Now().Add(refreshTTL)
	if _, err := app.DB.Exec(r.Context(), `
		INSERT INTO refresh_tokens (user_id, jti, user_agent, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, userID, jti, ua, ip, expiresAt); err != nil {
		return a.TokenPair{}, err
	}

	return a.TokenPair{AccessToken: access, RefreshToken: refresh}, nil
}

func (app *App) loadUser(r *http.Request, id string) UserDTO {
	var u UserDTO
	_ = app.DB.QueryRow(r.Context(), `
		SELECT id, email, username, display_name, created_at
		FROM users WHERE id=$1
	`, id).Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.CreatedAt)
	return u
}

func clientIP(r *http.Request) string {
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		parts := strings.Split(x, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func minutesFromEnv(k string, def int) time.Duration {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			return time.Duration(i) * time.Minute
		}
	}
	return time.Duration(def) * time.Minute
}
func daysFromEnv(k string, def int) time.Duration {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			return time.Duration(i) * 24 * time.Hour
		}
	}
	return time.Duration(def) * 24 * time.Hour
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": msg}})
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
