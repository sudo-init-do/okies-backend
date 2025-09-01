package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

type createWithdrawalReq struct {
	Amount int64 `json:"amount"` // kobo > 0
}
type withdrawalDTO struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	CreatedAt time.Time `json:"createdAt"`
}

func (app *App) CreateWithdrawal(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok { httpError(w, http.StatusUnauthorized, "not_authenticated"); return }

	var body createWithdrawalReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Amount <= 0 {
		httpError(w, http.StatusBadRequest, "invalid_request"); return
	}

	// wallets (user + system)
	var userWalletID, systemWalletID, systemUserID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, uid).Scan(&userWalletID); err != nil {
		httpError(w, http.StatusNotFound, "wallet_not_found"); return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM users WHERE email='system@okies.local'`).Scan(&systemUserID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_user_missing"); return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, systemUserID).Scan(&systemWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_wallet_missing"); return
	}

	tx, err := app.DB.Begin(r.Context())
	if err != nil { httpError(w, http.StatusInternalServerError, "tx_begin_error"); return }
	defer tx.Rollback(r.Context())

	// lock wallets deterministic
	wids := []string{userWalletID, systemWalletID}
	sort.Strings(wids)
	if _, err := tx.Exec(r.Context(), `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, wids); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error"); return
	}

	// check balance
	var balance int64
	if err := tx.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='credit' THEN amount ELSE -amount END),0)
		FROM ledger_entries WHERE wallet_id=$1
	`, userWalletID).Scan(&balance); err != nil {
		httpError(w, http.StatusInternalServerError, "db_error"); return
	}
	if balance < body.Amount {
		httpError(w, http.StatusBadRequest, "insufficient_funds"); return
	}

	// create tx 'withdrawal' & move funds to system (hold)
	var holdTxID, wID string
	err = tx.QueryRow(r.Context(), `
		WITH t AS (
		  INSERT INTO transactions (kind, amount, currency, metadata)
		  VALUES ('withdrawal', $1, 'NGN', '{}'::jsonb) RETURNING id
		)
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		SELECT t.id, $2, 'debit', $1 FROM t
		UNION ALL
		SELECT t.id, $3, 'credit', $1 FROM t
		RETURNING (SELECT id FROM t) AS tx_id
	`, body.Amount, userWalletID, systemWalletID).Scan(&holdTxID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "insert_hold_error"); return
	}

	// create withdrawal request linked to hold tx
	err = tx.QueryRow(r.Context(), `
		INSERT INTO withdrawals (user_id, amount, currency, status, tx_id)
		VALUES ($1, $2, 'NGN', 'pending', $3)
		RETURNING id
	`, uid, body.Amount, holdTxID).Scan(&wID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "insert_withdrawal_error"); return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error"); return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": withdrawalDTO{
		ID: wID, Status: "pending", Amount: body.Amount, Currency: "NGN", CreatedAt: time.Now(),
	}})
}

func (app *App) AdminApproveWithdrawal(w http.ResponseWriter, r *http.Request) {
	adminID, ok := getUserID(r)
	if !ok { httpError(w, http.StatusUnauthorized, "not_authenticated"); return }

	id := chi.URLParam(r, "id")
	if id == "" { httpError(w, http.StatusBadRequest, "invalid_id"); return }

	// mark as approved; funds already held in system wallet
	ct, err := app.DB.Exec(r.Context(), `
		UPDATE withdrawals SET status='approved', approved_by=$2, approved_at=now(), updated_at=now()
		WHERE id=$1 AND status='pending'
	`, id, adminID)
	if err != nil { httpError(w, http.StatusInternalServerError, "db_error"); return }
	// If no row updated, either missing or already processed
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"withdrawalId": id, "status": "approved", "rows": ct}})
}

func (app *App) AdminRejectWithdrawal(w http.ResponseWriter, r *http.Request) {
	adminID, ok := getUserID(r)
	if !ok { httpError(w, http.StatusUnauthorized, "not_authenticated"); return }

	id := chi.URLParam(r, "id")
	if id == "" { httpError(w, http.StatusBadRequest, "invalid_id"); return }

	// Load withdrawal + related info
	var userID string
	var amount int64
	var txID string
	err := app.DB.QueryRow(r.Context(), `
		SELECT user_id, amount, tx_id
		FROM withdrawals
		WHERE id=$1 AND status='pending'
	`, id).Scan(&userID, &amount, &txID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "not_found_or_already_processed"); return
	}
	if err != nil { httpError(w, http.StatusInternalServerError, "db_error"); return }

	// wallets
	var userWalletID, systemWalletID, systemUserID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, userID).Scan(&userWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "wallet_not_found"); return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM users WHERE email='system@okies.local'`).Scan(&systemUserID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_user_missing"); return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, systemUserID).Scan(&systemWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_wallet_missing"); return
	}

	tx, err := app.DB.Begin(r.Context())
	if err != nil { httpError(w, http.StatusInternalServerError, "tx_begin_error"); return }
	defer tx.Rollback(r.Context())

	// lock wallets
	wids := []string{userWalletID, systemWalletID}
	sort.Strings(wids)
	if _, err := tx.Exec(r.Context(), `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, wids); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error"); return
	}

	// Create reversal transaction: credit user, debit system
	var revTxID string
	err = tx.QueryRow(r.Context(), `
		WITH t AS (
		  INSERT INTO transactions (kind, amount, currency, metadata)
		  VALUES ('withdrawal', $1, 'NGN', '{"reversal": true}'::jsonb) RETURNING id
		)
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		SELECT t.id, $2, 'credit', $1 FROM t
		UNION ALL
		SELECT t.id, $3, 'debit', $1 FROM t
		RETURNING (SELECT id FROM t) AS tx_id
	`, amount, userWalletID, systemWalletID).Scan(&revTxID)
	if err != nil { httpError(w, http.StatusInternalServerError, "insert_reversal_error"); return }

	// mark rejected
	if _, err := tx.Exec(r.Context(), `
		UPDATE withdrawals
		SET status='rejected', approved_by=$2, approved_at=now(), updated_at=now()
		WHERE id=$1 AND status='pending'
	`, id, adminID); err != nil {
		httpError(w, http.StatusInternalServerError, "db_error"); return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error"); return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"withdrawalId": id, "status": "rejected"}})
}
