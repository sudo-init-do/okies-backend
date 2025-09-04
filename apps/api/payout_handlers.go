package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------- Types

type createDestReq struct {
	BankCode      string `json:"bankCode"`
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
	IsDefault     *bool  `json:"isDefault,omitempty"`
}

type destDTO struct {
	ID            string    `json:"id"`
	BankCode      string    `json:"bankCode"`
	AccountNumber string    `json:"accountNumber"`
	AccountName   string    `json:"accountName"`
	IsDefault     bool      `json:"isDefault"`
	CreatedAt     time.Time `json:"createdAt"`
}

type createWithdrawalReq struct {
	DestinationID string `json:"destinationId"`
	Amount        int64  `json:"amount"` // kobo
}

type withdrawalDTO struct {
	ID          string    `json:"id"`
	Destination string    `json:"destinationId"`
	Amount      int64     `json:"amount"`
	Status      string    `json:"status"`
	Reference   string    `json:"reference"`
	CreatedAt   time.Time `json:"createdAt"`
}

// ---------- Helpers

func (app *App) walletIDForUser(ctx context.Context, userID string) (string, error) {
	var wid string
	err := app.DB.QueryRow(ctx, `SELECT id FROM wallets WHERE user_id=$1`, userID).Scan(&wid)
	return wid, err
}

func (app *App) systemUserAndWallet(ctx context.Context) (string, string, error) {
	var sysID, wid string
	if err := app.DB.QueryRow(ctx, `SELECT id FROM users WHERE email='system@okies.local'`).Scan(&sysID); err != nil {
		return "", "", err
	}
	if err := app.DB.QueryRow(ctx, `SELECT id FROM wallets WHERE user_id=$1`, sysID).Scan(&wid); err != nil {
		return "", "", err
	}
	return sysID, wid, nil
}

// ---------- Payout Destinations

func (app *App) CreatePayoutDestination(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var body createDestReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.BankCode) == "" ||
		strings.TrimSpace(body.AccountNumber) == "" ||
		strings.TrimSpace(body.AccountName) == "" {
		httpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	isDefault := false
	if body.IsDefault != nil {
		isDefault = *body.IsDefault
	}

	ctx := r.Context()
	tx, err := app.DB.Begin(ctx)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "tx_begin_error")
		return
	}
	defer tx.Rollback(ctx)

	if isDefault {
		_, _ = tx.Exec(ctx, `UPDATE payout_destinations SET is_default=false WHERE user_id=$1`, uid)
	}

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO payout_destinations (user_id, bank_code, account_number, account_name, is_default)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id
	`, uid, body.BankCode, body.AccountNumber, body.AccountName, isDefault).Scan(&id); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_error")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"data": map[string]any{"id": id},
	})
}

func (app *App) ListPayoutDestinations(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	rows, err := app.DB.Query(r.Context(), `
		SELECT id, bank_code, account_number, account_name, is_default, created_at
		FROM payout_destinations
		WHERE user_id=$1
		ORDER BY created_at DESC
	`, uid)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	defer rows.Close()

	list := []destDTO{}
	for rows.Next() {
		var d destDTO
		if err := rows.Scan(&d.ID, &d.BankCode, &d.AccountNumber, &d.AccountName, &d.IsDefault, &d.CreatedAt); err != nil {
			httpError(w, http.StatusInternalServerError, "scan_error")
			return
		}
		list = append(list, d)
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (app *App) DeletePayoutDestination(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing_id")
		return
	}

	res, err := app.DB.Exec(r.Context(), `
		DELETE FROM payout_destinations
		WHERE id=$1 AND user_id=$2
	`, id, uid)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	if res.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true}})
}

// ---------- Withdrawals (User)

func (app *App) CreateWithdrawal(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var body createWithdrawalReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Amount <= 0 || strings.TrimSpace(body.DestinationID) == "" {
		httpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	ctx := r.Context()

	// Ensure destination belongs to the user
	var destUser string
	if err := app.DB.QueryRow(ctx, `
		SELECT user_id FROM payout_destinations WHERE id=$1
	`, body.DestinationID).Scan(&destUser); err != nil || destUser != uid {
		httpError(w, http.StatusBadRequest, "invalid_destination")
		return
	}

	userWid, err := app.walletIDForUser(ctx, uid)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "wallet_not_found")
		return
	}
	_, systemWid, err := app.systemUserAndWallet(ctx)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "system_wallet_missing")
		return
	}

	reference := "wd-" + uuid.NewString()
	idem := r.Header.Get("Idempotency-Key")
	if idem == "" {
		idem = reference
	}

	tx, err := app.DB.Begin(ctx)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "tx_begin_error")
		return
	}
	defer tx.Rollback(ctx)

	// lock wallets deterministically
	wids := []string{systemWid, userWid}
	sort.Strings(wids)
	if _, err := tx.Exec(ctx, `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, wids); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error")
		return
	}

	// idempotency check
	var existing string
	err = tx.QueryRow(ctx, `SELECT id FROM transactions WHERE idempotency_key=$1`, idem).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	if existing != "" {
		var payoutID string
		_ = tx.QueryRow(ctx, `SELECT id FROM payouts WHERE reference=$1`, idem).Scan(&payoutID)
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"payoutId": payoutID, "status": "pending"}})
		return
	}

	// reserve: debit user, credit system
	var txID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO transactions (idempotency_key, kind, amount, currency, metadata)
		VALUES ($1,'withdrawal_reserve',$2,'NGN','{}'::jsonb)
		RETURNING id
	`, idem, body.Amount).Scan(&txID); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_tx_error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		VALUES
		  ($1,$2,'debit',$3),
		  ($1,$4,'credit',$3)
	`, txID, userWid, body.Amount, systemWid); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_ledger_error")
		return
	}

	// create payout
	var payoutID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO payouts (user_id, destination_id, amount, status, reference)
		VALUES ($1,$2,$3,'pending',$4)
		RETURNING id
	`, uid, body.DestinationID, body.Amount, idem).Scan(&payoutID); err != nil {
		httpError(w, http.StatusInternalServerError, "insert_payout_error")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"data": map[string]any{
			"payoutId":  payoutID,
			"status":    "pending",
			"reference": idem,
		},
	})
}

func (app *App) ListMyWithdrawals(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	rows, err := app.DB.Query(r.Context(), `
		SELECT id, destination_id, amount, status, reference, created_at
		FROM payouts
		WHERE user_id=$1
		ORDER BY created_at DESC
		LIMIT 100
	`, uid)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	defer rows.Close()

	out := []withdrawalDTO{}
	for rows.Next() {
		var d withdrawalDTO
		if err := rows.Scan(&d.ID, &d.Destination, &d.Amount, &d.Status, &d.Reference, &d.CreatedAt); err != nil {
			httpError(w, http.StatusInternalServerError, "scan_error")
			return
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// ---------- Withdrawals (Admin)

func (app *App) AdminApproveWithdrawal(w http.ResponseWriter, r *http.Request) {
	if _, ok := getUserID(r); !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing_id")
		return
	}

	ctx := r.Context()
	var (
		userID, destID, status, reference string
		amount                            int64
	)
	if err := app.DB.QueryRow(ctx, `
		SELECT user_id, destination_id, amount, status, reference
		FROM payouts
		WHERE id = $1
	`, id).Scan(&userID, &destID, &amount, &status, &reference); err != nil {
		httpError(w, http.StatusNotFound, "payout_not_found")
		return
	}

	// already settled
	if status == "succeeded" {
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"status": "succeeded"}})
		return
	}

	// mark approved (idempotent)
	_, _ = app.DB.Exec(ctx, `UPDATE payouts SET status='approved', updated_at=now() WHERE id=$1`, id)

	// attempt bank transfer; webhook will finalize state
	if app.Flutterwave != nil {
		var bank, acct, name string
		if err := app.DB.QueryRow(ctx, `
			SELECT bank_code, account_number, account_name
			FROM payout_destinations
			WHERE id=$1
		`, destID).Scan(&bank, &acct, &name); err == nil && bank != "" && acct != "" {
			_ = app.Flutterwave.CreateTransfer(ctx, bank, acct, amount, "NGN", "Okies payout", reference, "")
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"status":    "approved",
			"payoutId":  id,
			"reference": reference,
		},
	})
}

func (app *App) AdminRejectWithdrawal(w http.ResponseWriter, r *http.Request) {
	if _, ok := getUserID(r); !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing_id")
		return
	}

	ctx := r.Context()
	var (
		userID, status, reference string
		amount                    int64
	)
	if err := app.DB.QueryRow(ctx, `
		SELECT user_id, status, reference, amount
		FROM payouts
		WHERE id=$1
	`, id).Scan(&userID, &status, &reference, &amount); err != nil {
		httpError(w, http.StatusNotFound, "payout_not_found")
		return
	}
	if status == "succeeded" {
		httpError(w, http.StatusBadRequest, "cannot_reject_succeeded")
		return
	}

	userWid, err := app.walletIDForUser(ctx, userID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "wallet_not_found")
		return
	}
	_, systemWid, err := app.systemUserAndWallet(ctx)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "system_wallet_missing")
		return
	}

	refundIdem := reference + ":rejected_refund"

	tx, err := app.DB.Begin(ctx)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "tx_begin_error")
		return
	}
	defer tx.Rollback(ctx)

	// lock payout
	if _, err := tx.Exec(ctx, `SELECT id FROM payouts WHERE id=$1 FOR UPDATE`, id); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_payout_error")
		return
	}
	// lock wallets
	wids := []string{systemWid, userWid}
	sort.Strings(wids)
	if _, err := tx.Exec(ctx, `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, wids); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error")
		return
	}

	// mark rejected
	_, _ = tx.Exec(ctx, `UPDATE payouts SET status='rejected', updated_at=now() WHERE id=$1`, id)

	// idempotent refund (reverse reserve)
	var exists string
	err = tx.QueryRow(ctx, `SELECT id FROM transactions WHERE idempotency_key=$1`, refundIdem).Scan(&exists)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	if exists == "" {
		var txID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO transactions (idempotency_key, kind, amount, currency, metadata)
			VALUES ($1,'withdrawal_refund',$2,'NGN','{}'::jsonb)
			RETURNING id
		`, refundIdem, amount).Scan(&txID); err != nil {
			httpError(w, http.StatusInternalServerError, "insert_tx_error")
			return
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
			VALUES
				($1,$2,'credit',$3), -- user gets funds back
				($1,$4,'debit',$3)   -- system pays back
		`, txID, userWid, amount, systemWid); err != nil {
			httpError(w, http.StatusInternalServerError, "insert_ledger_error")
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"status":    "rejected",
			"payoutId":  id,
			"reference": reference,
			"refunded":  true,
		},
	})
}
