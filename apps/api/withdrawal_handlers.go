package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// ==== Request/Response payloads ====

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

// ==== Helpers ====

func (app *App) latestActivePayoutDestination(r *http.Request, userID string) (accountNumber, bankCode, accountName string, ok bool) {
	err := app.DB.QueryRow(r.Context(), `
		SELECT account_number, bank_code, COALESCE(account_name, '')
		FROM payout_destinations
		WHERE user_id=$1 AND status='active'
		ORDER BY created_at DESC
		LIMIT 1
	`, userID).Scan(&accountNumber, &bankCode, &accountName)
	if err != nil {
		return "", "", "", false
	}
	return accountNumber, bankCode, accountName, true
}

// ==== Handlers ====

func (app *App) CreateWithdrawal(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var body createWithdrawalReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Amount <= 0 {
		httpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	// wallets (user + system)
	var userWalletID, systemWalletID, systemUserID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, uid).Scan(&userWalletID); err != nil {
		httpError(w, http.StatusNotFound, "wallet_not_found")
		return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM users WHERE email='system@okies.local'`).Scan(&systemUserID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_user_missing")
		return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, systemUserID).Scan(&systemWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_wallet_missing")
		return
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

	// check balance (sum(credits - debits))
	var balance int64
	if err := tx.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='credit' THEN amount ELSE -amount END),0)
		FROM ledger_entries
		WHERE wallet_id=$1
	`, userWalletID).Scan(&balance); err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	if balance < body.Amount {
		httpError(w, http.StatusBadRequest, "insufficient_funds")
		return
	}

	// move funds into "system" as a hold
	var holdTxID, wID string
	err = tx.QueryRow(r.Context(), `
		WITH t AS (
		  INSERT INTO transactions (kind, amount, currency, metadata)
		  VALUES ('withdrawal', $1, 'NGN', '{}'::jsonb)
		  RETURNING id
		)
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		SELECT t.id, $2, 'debit', $1 FROM t
		UNION ALL
		SELECT t.id, $3, 'credit', $1 FROM t
		RETURNING (SELECT id FROM t) AS tx_id
	`, body.Amount, userWalletID, systemWalletID).Scan(&holdTxID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "insert_hold_error")
		return
	}

	// create withdrawal request linked to the hold transaction
	err = tx.QueryRow(r.Context(), `
		INSERT INTO withdrawals (user_id, amount, currency, status, tx_id)
		VALUES ($1, $2, 'NGN', 'pending', $3)
		RETURNING id
	`, uid, body.Amount, holdTxID).Scan(&wID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "insert_withdrawal_error")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": withdrawalDTO{
		ID: wID, Status: "pending", Amount: body.Amount, Currency: "NGN", CreatedAt: time.Now(),
	}})
}

func (app *App) AdminApproveWithdrawal(w http.ResponseWriter, r *http.Request) {
	adminID, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "invalid_id")
		return
	}

	// Load withdrawal (allow retry if already approved but not yet transferred)
	var userID string
	var amount int64
	var status string
	err := app.DB.QueryRow(r.Context(), `
		SELECT user_id, amount, status
		FROM withdrawals
		WHERE id=$1
	`, id).Scan(&userID, &amount, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "withdrawal_not_found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	if status != "pending" && status != "approved" {
		httpError(w, http.StatusConflict, "already_processed")
		return
	}

	// ensure an active payout destination exists
	acctNo, bankCode, acctName, okDest := app.latestActivePayoutDestination(r, userID)
	if !okDest {
		httpError(w, http.StatusBadRequest, "payout_destination_missing")
		return
	}

	// check if a payout transfer already exists for this withdrawal
	var existingRef string
	_ = app.DB.QueryRow(r.Context(), `
		SELECT reference FROM payout_transfers WHERE withdrawal_id=$1
	`, id).Scan(&existingRef)
	if existingRef != "" {
		// already initiated; simply report current state
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"withdrawalId": id,
				"status":       "already_initiated",
				"reference":    existingRef,
			},
		})
		return
	}

	// mark as approved (if still pending)
	if status == "pending" {
		_, err = app.DB.Exec(r.Context(), `
			UPDATE withdrawals
			SET status='approved', approved_by=$2, approved_at=now(), updated_at=now()
			WHERE id=$1 AND status='pending'
		`, id, adminID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "db_error")
			return
		}
	}

	// Build Flutterwave transfer (amount must be in Naira, not kobo)
	if amount%100 != 0 {
		// enforce whole Naira payouts for now
		httpError(w, http.StatusBadRequest, "amount_not_whole_naira")
		return
	}
	naira := amount / 100

	ref := "wd-" + id // stable idempotent reference per withdrawal
	flw := NewFlutterwaveClient()

	reqBody := map[string]any{
		"account_bank":   bankCode,
		"account_number": acctNo,
		"amount":         naira,       // integer Naira
		"currency":       "NGN",
		"narration":      "Okies withdrawal for " + acctName,
		"reference":      ref,
	}

	// call Flutterwave
	resp, err := flw.do(r.Context(), "POST", "/v3/transfers", reqBody)
	if err != nil {
		// Do not unhold here; keep approved & held so admin can retry transfer.
		httpError(w, http.StatusBadGateway, "flutterwave_error")
		return
	}

	// persist payout_transfers
	rawReq, _ := json.Marshal(reqBody)
	rawResp, _ := json.Marshal(resp)

	_, err = app.DB.Exec(r.Context(), `
		INSERT INTO payout_transfers
			(withdrawal_id, reference, amount, currency, status, raw_request, raw_response)
		VALUES ($1, $2, $3, 'NGN', 'pending', $4, $5)
		ON CONFLICT (reference) DO NOTHING
	`, id, ref, amount, rawReq, rawResp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"withdrawalId": id,
			"status":       "transfer_initiated",
			"reference":    ref,
		},
	})
}

func (app *App) AdminRejectWithdrawal(w http.ResponseWriter, r *http.Request) {
	adminID, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "invalid_id")
		return
	}

	// Load withdrawal (ensure pending)
	var userID string
	var amount int64
	var txID string
	err := app.DB.QueryRow(r.Context(), `
		SELECT user_id, amount, tx_id
		FROM withdrawals
		WHERE id=$1 AND status='pending'
	`, id).Scan(&userID, &amount, &txID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "not_found_or_already_processed")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	// wallets: user + system
	var userWalletID, systemWalletID, systemUserID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, userID).Scan(&userWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "wallet_not_found")
		return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM users WHERE email='system@okies.local'`).Scan(&systemUserID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_user_missing")
		return
	}
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, systemUserID).Scan(&systemWalletID); err != nil {
		httpError(w, http.StatusInternalServerError, "system_wallet_missing")
		return
	}

	tx, err := app.DB.Begin(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "tx_begin_error")
		return
	}
	defer tx.Rollback(r.Context())

	// lock wallets
	wids := []string{userWalletID, systemWalletID}
	sort.Strings(wids)
	if _, err := tx.Exec(r.Context(), `SELECT id FROM wallets WHERE id = ANY($1) FOR UPDATE`, wids); err != nil {
		httpError(w, http.StatusInternalServerError, "lock_wallets_error")
		return
	}

	// reverse the hold: system debit, user credit
	var revTxID string
	err = tx.QueryRow(r.Context(), `
		WITH t AS (
		  INSERT INTO transactions (kind, amount, currency, metadata)
		  VALUES ('withdrawal', $1, 'NGN', '{"reversal": true}'::jsonb)
		  RETURNING id
		)
		INSERT INTO ledger_entries (tx_id, wallet_id, direction, amount)
		SELECT t.id, $2, 'credit', $1 FROM t
		UNION ALL
		SELECT t.id, $3, 'debit', $1 FROM t
		RETURNING (SELECT id FROM t) AS tx_id
	`, amount, userWalletID, systemWalletID).Scan(&revTxID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "insert_reversal_error")
		return
	}

	// mark withdrawal rejected
	if _, err := tx.Exec(r.Context(), `
		UPDATE withdrawals
		SET status='rejected', approved_by=$2, approved_at=now(), updated_at=now()
		WHERE id=$1 AND status='pending'
	`, id, adminID); err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpError(w, http.StatusInternalServerError, "tx_commit_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{"withdrawalId": id, "status": "rejected"},
	})
}

func (app *App) ListMyWithdrawals(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	limit := 20
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, err := app.DB.Query(r.Context(), `
		SELECT id, status, amount, currency, created_at
		FROM withdrawals
		WHERE user_id=$1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, uid, limit, offset)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	defer rows.Close()

	type item struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		Amount    int64     `json:"amount"`
		Currency  string    `json:"currency"`
		CreatedAt time.Time `json:"createdAt"`
	}
	var out []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Status, &it.Amount, &it.Currency, &it.CreatedAt); err != nil {
			httpError(w, http.StatusInternalServerError, "scan_error")
			return
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   out,
		"paging": map[string]any{"limit": limit, "offset": offset},
	})
}
