package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/store"
)

type modelVariant struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	ID       string `json:"id"`
}

func (s *Server) handleUnifiedModels(w http.ResponseWriter, r *http.Request) {
	p := keyFromRequest(r).Permissions
	if r.URL.Query().Get("view") == "legacy" {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.modelCatalog(p)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.unifiedModelCatalog(p)})
}

func (s *Server) unifiedModelCatalog(p store.KeyPermissions) []map[string]any {
	groups := map[string]map[string]modelVariant{}
	for _, item := range s.pool.Items() {
		if !item.Enabled || !p.AllowsAccount(item.ID) {
			continue
		}
		for _, native := range item.Models {
			if strings.TrimSpace(native) == "" || !s.keyAllowsModel(p, native, item.Provider) {
				continue
			}
			id := pool.CanonicalModelID(native)
			// These names select an upstream policy, not a fixed shared model.
			switch id {
			case "auto", "default", "speed", "lite", "balanced-model", "fast-model", "deep-model":
				id = item.Provider + "/" + id
			}
			if groups[id] == nil {
				groups[id] = map[string]modelVariant{}
			}
			qualified := item.Provider + "/" + native
			groups[id][qualified] = modelVariant{Provider: item.Provider, Model: native, ID: qualified}
		}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		variants := make([]modelVariant, 0, len(groups[id]))
		providers := map[string]bool{}
		for _, v := range groups[id] {
			variants = append(variants, v)
			providers[v.Provider] = true
		}
		sort.Slice(variants, func(i, j int) bool { return variants[i].ID < variants[j].ID })
		channels := make([]string, 0, len(providers))
		for channel := range providers {
			channels = append(channels, channel)
		}
		sort.Strings(channels)
		owner := "relaypool"
		if len(channels) == 1 {
			owner = channels[0]
		}
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": owner, "providers": channels, "variants": variants})
	}
	return data
}
