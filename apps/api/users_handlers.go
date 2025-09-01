package main

import (
	"net/http"
	"strings"
)

type UserMini struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	Username    *string `json:"username,omitempty"`
	DisplayName *string `json:"displayName,omitempty"`
}

func (app *App) SearchUsers(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("query"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"data": []UserMini{}})
		return
	}
	qpat := "%" + strings.ToLower(q) + "%"
	rows, err := app.DB.Query(r.Context(), `
		SELECT id, email, username, display_name
		FROM users
		WHERE lower(email) LIKE $1 OR lower(username) LIKE $1
		ORDER BY created_at DESC
		LIMIT 20
	`, qpat)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "db_error")
		return
	}
	defer rows.Close()

	var out []UserMini
	for rows.Next() {
		var u UserMini
		if err := rows.Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName); err != nil {
			httpError(w, http.StatusInternalServerError, "scan_error")
			return
		}
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
