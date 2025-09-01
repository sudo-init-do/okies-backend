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

type createGiftReq struct {
	RecipientUserID string `json:"recipientUserId"`
	Amount          int64  `json:"amount"` // kobo > 0
	Note            string `json:"note,omitempty"`
}
type giftResp struct {
	GiftID string `json:"giftId"`
	Status string `json:"status"`
}

func (app *App) CreateGift(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}
	var body createGiftReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RecipientUserID == "" || body.Amount <= 0 {
		httpError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if body.RecipientUserID == uid {
		httpError(w, http.StatusBadRequest, "cannot_gift_self")
		return
	}

	// Resolve wallets
	var senderWalletID, recipientWalletID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, uid).Scan(&senderWalletID); err != nil {
		httpError(w, http.StatusNotFound, "wallet_not_found")
		return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, body.RecipientUserID).Scan(&recipientWalletID); err != nil {
		httpError(w, http.StatusBadRequest, "recipient_wallet_not_found")
		return
	}

	// Idempotency
	idem := r.Header.Get("Idempotency-Key")
	if idem == "" {
		idem = uuid.NewString()
	}
	idem = strings.TrimSpace(idem)

	tx, err := app.DB.Begin(r.Context())
	if err != nil { httpError(w, http.StatusInternalServerError, "tx_begin_error"); return }
	defer tx.Rollback(r.Context())

	// Lock both wallets in deterministic order to avoid deadlocks
	walletIDs := []string{senderWalletID, recipientWalletID}
	sort.Strings(walletIDs)
	if _, err := tx.Exec(r.Context(), `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, walletIDs); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error"); return
	}

	// Idempotency check
	var existing string
	err = tx.QueryRow(r.Context(), `SELECT id FROM transactions WHERE idempotency_key=$1`, idem).Scan(&existing)
	if err == nil && existing != "" {
		writeJSON(w, http.StatusOK, map[string]any{"data": giftResp{GiftID: existing, Status: "succeeded"}})
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	// Balance check (sender)
	var balance int64
	if err := tx.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='credit' THEN amount ELSE -amount END),0)
		FROM ledger_entries WHERE wallet_id=$1
	`, senderWalletID).Scan(&balance); err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	if balance < body.Amount {
		httpError(w, http.StatusBadRequest, "insufficient_funds")
		return
	}

	// Insert transaction
	var txID string
	var meta any = nil
	err = tx.QueryRow(r.Context(), `
		INSERT INTO transactions (idempotency_key, kind, amount, currency, metadata)
		VALUES ($1,'gift',$2,'NGN', COALESCE($3::jsonb, '{}'::jsonb))
		RETURNING id
	`, idem, body.Amount, meta).Scan(&txID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "insert_tx_error")
		return
	}

	// Ledger: debit sender, credit recipient
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		VALUES ($1,$2,'debit',$3), ($1,$4,'credit',$3)
	`, txID, senderWalletID, body.Amount, recipientWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_ledger_error")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": giftResp{GiftID: txID, Status: "succeeded"}})
}
