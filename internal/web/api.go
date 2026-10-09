package web

import (
	"encoding/json"
	"net/http"
)

// apiProvider is a provider as the API tells of it.
type apiProvider struct {
	ID      string `json:"id"` // names it in the API's addresses, such as "tvp"
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func describe(p Provider) apiProvider {
	return apiProvider{p.Tuner.Provider().Name(), p.Tuner.Name(), p.Tuner.Enabled()}
}

// listProviders lists the providers, and whether each is enabled.
func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	providers := make([]apiProvider, len(h.Providers))
	for i, p := range h.Providers {
		providers[i] = describe(p)
	}
	writeJSON(w, providers)
}

// patchProvider enables or disables a provider, as the request's body has
// it: {"enabled": false}. It answers with the provider as it then is.
func (h *Handler) patchProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := h.find(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var change struct{ Enabled *bool }
	if err := json.NewDecoder(r.Body).Decode(&change); err != nil || change.Enabled == nil {
		http.Error(w, `want a body like {"enabled": false}`, http.StatusBadRequest)
		return
	}
	if err := h.setEnabled(r.Context(), p, *change.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, describe(p))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
