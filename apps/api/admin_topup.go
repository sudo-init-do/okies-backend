// apps/api/admin_topup.go
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type adminTopupReq struct {
	UserID string `json:"userId"`
	Amount int64  `json:"amount"`           // kobo > 0
	Reason string `json:"reason,omitempty"` // optional memo
}

func (app *App) AdminTopup(w http.ResponseWriter, r *http.Request) {
	adminID, ok := getUserID(r) // RequireAdmin already enforced
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var body adminTopupReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" || body.Amount <= 0 {
		httpError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// guardrail: max single top-up = ₦5,000,000
	if body.Amount > 500000000 {
		httpError(w, http.StatusBadRequest, "amount_too_large")
		return
	}

	// resolve wallets
	var userWalletID, systemWalletID, systemUserID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM users WHERE email='system@okies.local'`).Scan(&systemUserID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_user_missing")
		return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, body.UserID).Scan(&userWalletID); err != nil {
		httpError(w, http.StatusBadRequest, "target_wallet_not_found")
		return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, systemUserID).Scan(&systemWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_wallet_missing")
		return
	}
	if userWalletID == systemWalletID {
		httpError(w, http.StatusBadRequest, "invalid_target_wallet")
		return
	}

	// idempotency
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idem == "" {
		idem = uuid.NewString()
	}

	tx, err := app.DB.Begin(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "tx_begin_error")
		return
	}
	defer tx.Rollback(r.Context())

	// lock wallets deterministically
	wids := []string{userWalletID, systemWalletID}
	sort.Strings(wids)
	if _, err := tx.Exec(r.Context(), `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, wids); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error")
		return
	}

	// idempotency check
	var existing string
	err = tx.QueryRow(r.Context(), `SELECT id FROM transactions WHERE idempotency_key=$1`, idem).Scan(&existing)
	if err == nil && existing != "" {
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"topupId": existing, "status": "succeeded"}})
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	// insert transaction with metadata
	var txID string
	err = tx.QueryRow(r.Context(), `
		INSERT INTO transactions (idempotency_key, kind, amount, currency, metadata)
		VALUES (
			$1,
			'topup',
			$2,
			'NGN',
			jsonb_build_object(
				'reason',        COALESCE(NULLIF($3, ''), NULL),
				'adminUserId',   $4,
				'targetUserId',  $5
			)
		)
		RETURNING id
	`, idem, body.Amount, body.Reason, adminID, body.UserID).Scan(&txID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "insert_tx_error")
		return
	}

	// ledger: debit system, credit user
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		VALUES
		  ($1,$2,'debit',  $4),
		  ($1,$3,'credit', $4)
	`, txID, systemWalletID, userWalletID, body.Amount); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_ledger_error")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]any{"topupId": txID, "status": "succeeded"}})
}
