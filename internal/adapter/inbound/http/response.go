package http

import (
	"encoding/json"
	"net/http"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error:   http.StatusText(status),
		Message: message,
	})
}
