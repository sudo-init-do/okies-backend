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
	Amount int64  `json:"amount"`
	Reason string `json:"reason,omitempty"`
}

func (app *App) AdminTopup(w http.ResponseWriter, r *http.Request) {
	_, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var body adminTopupReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.UserID) == "" || body.Amount <= 0 {
		httpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	var systemUserID, systemWalletID, userWalletID string
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

	wids := []string{systemWalletID, userWalletID}
	sort.Strings(wids)
	if _, err := tx.Exec(r.Context(), `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, wids); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error")
		return
	}

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

	var txID string
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO transactions (idempotency_key, kind, amount, currency, metadata)
		VALUES ($1,'topup',$2,'NGN','{}'::jsonb)
		RETURNING id
	`, idem, body.Amount).Scan(&txID); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_tx_error")
		return
	}

	if _, err := tx.Exec(r.Context(), `
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		VALUES ($1,$2,'debit',$3), ($1,$4,'credit',$3)
	`, txID, systemWalletID, body.Amount, userWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_ledger_error")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]any{"topupId": txID, "status": "succeeded"}})
}
