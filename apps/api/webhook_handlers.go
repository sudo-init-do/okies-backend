// apps/api/webhook_handlers.go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

func (app *App) FlutterwaveWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("FLW_WEBHOOK_HASH")
	if secret == "" || r.Header.Get("verif-hash") != secret {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	raw, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)

	event := getString(payload, "event") // e.g. "transfer.completed"
	data := getMap(payload, "data")
	ref := getString(data, "reference")
	status := strings.ToLower(getString(data, "status")) // SUCCESSFUL/FAILED/PENDING → normalize

	// Store raw webhook
	_, _ = app.DB.Exec(r.Context(), `
		INSERT INTO webhook_events (provider, event_type, reference, payload)
		VALUES ('flutterwave', $1, NULLIF($2,''), $3::jsonb)
	`, event, ref, string(raw))

	if ref != "" {
		// Update payout transfer
		_, _ = app.DB.Exec(r.Context(), `
			UPDATE payout_transfers
			SET status=$1, raw_response=$2::jsonb, updated_at=now()
			WHERE reference=$3
		`, status, string(raw), ref)

		// Update withdrawal status (simple mapping)
		switch status {
		case "successful", "success", "completed":
			_, _ = app.DB.Exec(r.Context(), `
				UPDATE withdrawals
				SET status='paid', updated_at=now()
				WHERE id=(SELECT withdrawal_id FROM payout_transfers WHERE reference=$1)
				  AND status IN ('approved','pending')
			`, ref)
			// (Optional) post a system outflow ledger entry here.

		case "failed", "error":
			_, _ = app.DB.Exec(r.Context(), `
				UPDATE withdrawals
				SET status='failed', updated_at=now()
				WHERE id=(SELECT withdrawal_id FROM payout_transfers WHERE reference=$1)
				  AND status IN ('approved','pending')
			`, ref)
			// (Optional) auto-reverse the hold here.
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// helpers
func getString(m map[string]any, k string) string {
	if m == nil {
	return ""
	}
	if v, ok := m[k]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	return ""
}
func getMap(m map[string]any, k string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[k]; ok && v != nil {
		if mm, ok := v.(map[string]any); ok {
			return mm
		}
	}
	return nil
}
