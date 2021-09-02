package dshttp

import (
	"encoding/json"
	"net/http"
)

// WriteJSONResponse writes some JSON as an HTTP response.
func WriteJSONResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")

	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// We ignore errors here, because we cannot do anything about them.
	// Write will trigger sending Status code, so we cannot send a different status code afterwards.
	// Also this isn't internal error, but error communicating with client.
	_, _ = w.Write(data)
}
