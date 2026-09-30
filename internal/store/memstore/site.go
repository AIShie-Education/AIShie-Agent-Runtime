package memstore

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// SiteSettings lists every setting set, by name.
func (s *Store) SiteSettings(_ context.Context) ([]store.SiteSetting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.SiteSetting, 0, len(s.settings))
	for _, st := range s.settings {
		st.Value = slices.Clone(st.Value)
		out = append(out, st)
	}
	slices.SortFunc(out, func(x, y store.SiteSetting) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// PutSiteSetting sets st, in place of what was set.
func (s *Store) PutSiteSetting(_ context.Context, st store.SiteSetting) error {
	st, err := store.CheckSiteSetting(st)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Value, st.UpdatedAt = slices.Clone(st.Value), s.orNow(st.UpdatedAt)
	s.settings[st.Name] = st
	s.rev++
	return nil
}

// DeleteSiteSetting unsets the setting; one not set is nothing, and moves
// the revision on no more than pgstore's does.
func (s *Store) DeleteSiteSetting(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.settings[name]; !ok {
		return nil
	}
	delete(s.settings, name)
	s.rev++
	return nil
}

// SchoolOffers lists the offers, by id.
func (s *Store) SchoolOffers(_ context.Context) ([]store.SchoolOffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.SchoolOffer, 0, len(s.offers))
	for _, o := range s.offers {
		out = append(out, o)
	}
	slices.SortFunc(out, func(x, y store.SchoolOffer) int { return strings.Compare(x.ID, y.ID) })
	return out, nil
}

// SchoolOffer is the offer id, or store.ErrNotFound.
func (s *Store) SchoolOffer(_ context.Context, id string) (*store.SchoolOffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.offers[id]
	if !ok {
		return nil, fmt.Errorf("school offer %s: %w", id, store.ErrNotFound)
	}
	return &o, nil
}

// storeOfferKey stores an offer's new key, refusing an id taken. Called
// with the lock held.
func (s *Store) storeOfferKey(o store.SchoolOffer, key store.Secret) error {
	if err := store.CheckOfferKey(o, key); err != nil {
		return err
	}
	if _, ok := s.secrets[key.ID]; ok {
		return fmt.Errorf("secret %s: %w", key.ID, store.ErrExists)
	}
	key.CreatedAt = s.orNow(key.CreatedAt)
	s.secrets[key.ID] = copySecret(key)
	return nil
}

// CreateSchoolOffer stores o at version 1 with its key.
func (s *Store) CreateSchoolOffer(_ context.Context, o store.SchoolOffer, key store.Secret) (*store.SchoolOffer, error) {
	if err := store.CheckSchoolOffer(o); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.offers[o.ID]; ok {
		return nil, fmt.Errorf("school offer %s: %w", o.ID, store.ErrExists)
	}
	if err := s.storeOfferKey(o, key); err != nil {
		return nil, err
	}
	o.Version, o.CreatedAt = 1, s.orNow(o.CreatedAt)
	o.UpdatedAt = o.CreatedAt
	if o.UpdatedBy == "" {
		o.UpdatedBy = o.CreatedBy
	}
	s.offers[o.ID] = o
	s.rev++
	return &o, nil
}

// UpdateSchoolOffer writes o over the offer of its id, at the version it
// names when it names one.
func (s *Store) UpdateSchoolOffer(_ context.Context, o store.SchoolOffer, key ...store.Secret) (*store.SchoolOffer, error) {
	if err := store.CheckSchoolOffer(o); err != nil {
		return nil, err
	}
	if len(key) > 1 {
		return nil, fmt.Errorf("store: school offer %s: one key at most", o.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.offers[o.ID]
	switch {
	case !ok:
		return nil, fmt.Errorf("school offer %s: %w", o.ID, store.ErrNotFound)
	case o.Version != 0 && old.Version != o.Version:
		return nil, fmt.Errorf("school offer %s at version %d: %w", o.ID, o.Version, store.ErrConflict)
	case len(key) == 0 && o.KeySecretID != old.KeySecretID:
		return nil, fmt.Errorf("store: school offer %s: a new key must be given with the write", o.ID)
	}
	if len(key) == 1 {
		if err := s.storeOfferKey(o, key[0]); err != nil {
			return nil, err
		}
		if old.KeySecretID != o.KeySecretID {
			delete(s.secrets, old.KeySecretID)
		}
	}
	o.Version, o.CreatedBy, o.CreatedAt, o.UpdatedAt = old.Version+1, old.CreatedBy, old.CreatedAt, s.clock()
	s.offers[o.ID] = o
	s.rev++
	return &o, nil
}

// DeleteSchoolOffer destroys the offer and its key, at version when it is
// not 0.
func (s *Store) DeleteSchoolOffer(_ context.Context, id string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.offers[id]
	switch {
	case !ok:
		return fmt.Errorf("school offer %s: %w", id, store.ErrNotFound)
	case version != 0 && o.Version != version:
		return fmt.Errorf("school offer %s at version %d: %w", id, version, store.ErrConflict)
	}
	delete(s.offers, id)
	delete(s.secrets, o.KeySecretID)
	s.rev++
	return nil
}
