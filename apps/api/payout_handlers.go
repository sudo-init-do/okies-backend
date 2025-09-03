package main

import (
	"encoding/json"
	"net/http"
)

type createDestinationReq struct {
	AccountNumber string `json:"accountNumber"`
	BankCode      string `json:"bankCode"`
	AccountName   string `json:"accountName"`
}

func (app *App) CreatePayoutDestination(w http.ResponseWriter, r *http.Request) {
	uid, ok := getUserID(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not_authenticated")
		return
	}

	var body createDestinationReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	flw := NewFlutterwaveClient()
	resp, err := flw.do(r.Context(), "POST", "/v3/beneficiaries", map[string]any{
		"account_number": body.AccountNumber,
		"account_bank":   body.BankCode,
		"beneficiary_name": body.AccountName,
		"currency":       "NGN",
	})
	if err != nil {
		httpError(w, http.StatusBadGateway, "flutterwave_error")
		return
	}

	data := resp["data"].(map[string]any)
	recipientCode := data["id"]

	_, err = app.DB.Exec(r.Context(), `
		INSERT INTO payout_destinations (user_id, account_number, bank_code, account_name, recipient_code)
		VALUES ($1,$2,$3,$4,$5)
	`, uid, body.AccountNumber, body.BankCode, body.AccountName, recipientCode)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"data": map[string]any{
			"recipientCode": recipientCode,
		},
	})
}
