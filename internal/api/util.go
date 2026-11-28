package api

import (
	"context"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// siteSetting returns one named setting, or nil when it has never been set.
func (s *Server) siteSetting(ctx context.Context, name string) (*store.SiteSetting, error) {
	all, err := s.o.Store.SiteSettings(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, nil
}

// strPtr is x, or nil for its empty value.
func strPtr(x string) *string {
	if x == "" {
		return nil
	}
	return &x
}

// nonNil returns xs, or an empty list for JSON's [] rather than null.
func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
