package main

import (
	"net/http"
	"strconv"
)

type WalletDTO struct {
	Balance  int64  `json:"balance"`  // kobo
	Currency string `json:"currency"` // "NGN"
}

type TxDTO struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	AmountDelta int64  `json:"amountDelta"` // +credit / -debit for THIS wallet
	Currency    string `json:"currency"`
	CreatedAt   string `json:"createdAt"`
}

func (app *App) GetWallet(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var walletID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, uid).Scan(&walletID); err != nil {
		httpError(w, http.StatusNotFound, "wallet_not_found")
		return
	}

	var balance int64
	if err := app.DB.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='credit' THEN amount ELSE -amount END),0)
		FROM ledger_entries
		WHERE wallet_id=$1
	`, walletID).Scan(&balance); err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": WalletDTO{Balance: balance, Currency: "NGN"}})
}

func (app *App) ListWalletTransactions(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var walletID string
	if err := app.DB.QueryRow(r.Context(), `SELECT id FROM wallets WHERE user_id=$1`, uid).Scan(&walletID); err != nil {
		httpError(w, http.StatusNotFound, "wallet_not_found")
		return
	}

	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, err := app.DB.Query(r.Context(), `
		SELECT t.id, t.kind,
		       COALESCE(SUM(CASE WHEN le.wallet_id=$1 AND le.direction='credit' THEN le.amount ELSE -le.amount END),0) AS delta,
		       t.currency,
		       to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM transactions t
		JOIN ledger_entries le ON le.tx_id = t.id
		WHERE le.wallet_id = $1
		GROUP BY t.id
		ORDER BY t.created_at DESC
		LIMIT $2 OFFSET $3
	`, walletID, limit, offset)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	defer rows.Close()

	var out []TxDTO
	for rows.Next() {
		var t TxDTO
		if err := rows.Scan(&t.ID, &t.Kind, &t.AmountDelta, &t.Currency, &t.CreatedAt); err != nil {
			httpError(w, http.StatusInternalServerError, "scan_error")
			return
		}
		out = append(out, t)
	}
	if rows.Err() != nil {
		httpError(w, http.StatusInternalServerError, "rows_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": out, "paging": map[string]any{"limit": limit, "offset": offset}})
}
