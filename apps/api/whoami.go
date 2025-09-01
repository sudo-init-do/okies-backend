package main

import "net/http"

func (app *App) WhoAmI(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r)
	role, _ := getUserRole(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"userId": uid,
		"role":   role,
	})
}
