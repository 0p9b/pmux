package model

import (
	"context"
	"time"
	"github.com/0p9b/pmux/internal/domain/management"
)

type CatalogEntry struct {
	ID string `json:"id"`
	Owner string `json:"owner,omitempty"`
	Providers []management.ProviderID `json:"providers"`
	Available bool `json:"available"`
	Accounts int `json:"accounts"`
	Favorite bool `json:"favorite"`
	Source string `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	Stale bool `json:"stale"`
}

type ModelCatalog interface {
	List(context.Context) ([]CatalogEntry, error)
	Refresh(context.Context) ([]CatalogEntry, error)
	Attribution(context.Context) (map[string][]management.ProviderID, error)
}
