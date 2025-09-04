package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- Minimal client placeholder (safe no-op until you wire real HTTP) ---
type FlutterwaveClient interface {
	CreateTransfer(ctx context.Context, bankCode, accountNumber string, amount int64, currency, narration, reference, callbackURL string) error
}

type noopFlutterwave struct{}

func (noopFlutterwave) CreateTransfer(ctx context.Context, bankCode, accountNumber string, amount int64, currency, narration, reference, callbackURL string) error {
	return nil
}

func NewFlutterwaveClient(baseURL, secretKey, encKey string) (FlutterwaveClient, error) {
	if strings.TrimSpace(secretKey) == "" {
		return noopFlutterwave{}, nil
	}
	return noopFlutterwave{}, nil
}

// --- Webhook payload ---
type flwWebhook struct {
	Event string `json:"event"`
	Data  struct {
		Reference string `json:"reference"`
		Status    string `json:"status"`
		Amount    int64  `json:"amount"`
		Currency  string `json:"currency"`
	} `json:"data"`
}

// POST /v1/webhooks/flutterwave
// Verify with header `verif-hash` against env FLW_WEBHOOK_HASH.
// Accepts either direct equality or HMAC-SHA256(secret, rawBody) as hex.
func (app *App) FlutterwaveWebhook(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimSpace(os.Getenv("FLW_WEBHOOK_HASH"))
	verif := strings.TrimSpace(r.Header.Get("verif-hash"))
	if secret == "" || verif == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad_payload", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	// direct match or HMAC
	valid := (verif == secret)
	if !valid {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		sum := hex.EncodeToString(mac.Sum(nil))
		valid = (verif == sum)
	}
	if !valid {
		http.Error(w, "bad_signature", http.StatusForbidden)
		return
	}

	var evt flwWebhook
	if err := json.Unmarshal(body, &evt); err != nil {
		http.Error(w, "bad_payload", http.StatusBadRequest)
		return
	}

	// Handle transfer outcome
	if evt.Event == "transfer.completed" || evt.Event == "transfer.failed" {
		status := "succeeded"
		if strings.ToUpper(evt.Data.Status) != "SUCCESSFUL" {
			status = "failed"
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := app.DB.Exec(ctx, `
			UPDATE payouts
			SET status = $1, updated_at = now()
			WHERE reference = $2
		`, status, evt.Data.Reference); err != nil {
			http.Error(w, "db_error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}
