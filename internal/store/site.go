package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// What the site's administrators change through the API (docs/design.md
// §11.5), where the operator's environment and runtime.yaml set only the
// ceiling: the site's settings, by name, and the offers of the school's
// plan they make, each with the school's key sealed. Every write to either
// moves the registry's revision on (RegistryRev), and pgstore tells every
// listener (LISTEN aishie_registry), so that every worker puts it in force
// as it does a hosted agent's change, without a restart.

// SiteSetting is one of the site's settings: its value is a JSON object,
// which the API writes and the registry reads (config.SiteOCR,
// config.SiteQuotas).
type SiteSetting struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
	// UpdatedBy is the Core actor who wrote it.
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// The site's settings.
const (
	// SettingOCR is whether OCR runs, and in which languages.
	SettingOCR = "ocr"
	// SettingSchoolQuotas are the school plan's quotas in answers, in
	// place of runtime.yaml's.
	SettingSchoolQuotas = "school_quotas"
)

// SchoolTenantID is the tenant of the school's keys the site's
// administrators give its offers: the school's, and no owner's (ten_…).
const SchoolTenantID = "school"

// SchoolOffer is one model of the school's plan (the product owner's D8)
// that the site's administrators made through the API, beside those of
// runtime.yaml: its id, which owners choose it by, the label they are
// shown, the model section it stands for, made by the API from a
// provider's offer (its official endpoint), and the school's key, sealed.
type SchoolOffer struct {
	// ID is letters, digits, '_' and '-', at most 64, as runtime.yaml's
	// offers' are, and none of theirs.
	ID       string `json:"id"`
	Label    string `json:"label"`
	Adapter  string `json:"adapter"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// BaseURL is the endpoint the API made from the provider's offer, ""
	// for the adapter's own; Region is Bedrock's.
	BaseURL string `json:"base_url"`
	Region  string `json:"region"`
	// MaxOutputTokens is 0 for the runtime's default, and ReasoningEffort
	// "" for none.
	MaxOutputTokens int    `json:"max_output_tokens"`
	ReasoningEffort string `json:"reasoning_effort"`
	// Enabled is whether the school offers it now: an offer turned off is
	// kept, key and all, and offered again once turned on.
	Enabled bool `json:"enabled"`
	// KeySecretID is the school's key, sealed: a model_key of
	// SchoolTenantID, which goes with the offer. KeyHint is what may be
	// shown of it, and KeyTested whether it passed a trial of this model
	// when it was given: false when the trial was skipped, or the model
	// changed since.
	KeySecretID string `json:"key_secret_id"`
	KeyHint     string `json:"key_hint"`
	KeyTested   bool   `json:"key_tested"`
	// Version moves on with every write.
	Version int `json:"version"`
	// CreatedBy and UpdatedBy are Core actors.
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Site is what the site's administrators change through the API.
type Site interface {
	// SiteSettings lists every setting set, by name.
	SiteSettings(ctx context.Context) ([]SiteSetting, error)
	// PutSiteSetting sets s, in place of what was set. A zero UpdatedAt
	// is the store's now.
	PutSiteSetting(ctx context.Context, s SiteSetting) error
	// DeleteSiteSetting unsets the setting; one not set is nothing.
	DeleteSiteSetting(ctx context.Context, name string) error

	// SchoolOffers lists the offers, by id.
	SchoolOffers(ctx context.Context) ([]SchoolOffer, error)
	// SchoolOffer is the offer id, or ErrNotFound.
	SchoolOffer(ctx context.Context, id string) (*SchoolOffer, error)
	// CreateSchoolOffer stores o at version 1 with its key, which must be
	// the one it refers to, in one transaction, and returns it as stored:
	// an id taken is ErrExists. A zero CreatedAt is the store's now.
	CreateSchoolOffer(ctx context.Context, o SchoolOffer, key Secret) (*SchoolOffer, error)
	// UpdateSchoolOffer writes o over the offer of its id, but its
	// creation, and returns it at the next version: whatever its version
	// when o.Version is 0, and otherwise only if it is still at o.Version
	// (If-Match), ErrConflict when it has been written since; ErrNotFound
	// when it is gone. A key given is its new one, stored in the same
	// transaction, in which the one it replaces is destroyed.
	UpdateSchoolOffer(ctx context.Context, o SchoolOffer, key ...Secret) (*SchoolOffer, error)
	// DeleteSchoolOffer destroys the offer and its key in one
	// transaction: whatever its version when version is 0, and otherwise
	// only at version, ErrConflict when it has moved on; ErrNotFound when
	// it is not there.
	DeleteSchoolOffer(ctx context.Context, id string, version int) error
}

var (
	settingNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	offerIDRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// IsOfferID reports whether id has the shape of an offer's id.
func IsOfferID(id string) bool { return offerIDRe.MatchString(id) }

// CheckSiteSetting refuses a setting a store must not keep: a name not of
// lower-case letters, digits and '_', or a value that is not a JSON
// object. It returns the setting with its value as stored.
func CheckSiteSetting(s SiteSetting) (SiteSetting, error) {
	if !settingNameRe.MatchString(s.Name) {
		return s, errors.New("store: site setting: a name of lower-case letters, digits and '_' required")
	}
	if len(s.Value) == 0 {
		return s, fmt.Errorf("store: site setting %s: a value required", s.Name)
	}
	v, err := jsonObject(s.Value)
	if err != nil {
		return s, fmt.Errorf("store: site setting %s: the value must be a JSON object", s.Name)
	}
	s.Value = v
	return s, nil
}

// CheckSchoolOffer refuses an offer a store must not keep: without its id
// (IsOfferID), label, adapter, provider, model or key, or with a negative
// output bound.
func CheckSchoolOffer(o SchoolOffer) error {
	var bad []string
	if !IsOfferID(o.ID) {
		bad = append(bad, "id (letters, digits, '_' and '-', at most 64)")
	}
	for _, f := range []struct{ name, v string }{{"label", o.Label}, {"adapter", o.Adapter}, {"provider", o.Provider}, {"model", o.Model}} {
		if strings.TrimSpace(f.v) == "" {
			bad = append(bad, f.name)
		}
	}
	if !IsSecretID(o.KeySecretID) {
		bad = append(bad, "key_secret_id")
	}
	if o.MaxOutputTokens < 0 {
		bad = append(bad, "max_output_tokens of zero or more")
	}
	if len(bad) > 0 {
		return fmt.Errorf("store: school offer: %s required", strings.Join(bad, ", "))
	}
	return nil
}

// CheckOfferKey holds an offer's new key to it: the secret it refers to,
// a model_key of SchoolTenantID.
func CheckOfferKey(o SchoolOffer, key Secret) error {
	if err := CheckSecret(key); err != nil {
		return err
	}
	if key.ID != o.KeySecretID || key.Kind != SecretModelKey || key.TenantID != SchoolTenantID {
		return fmt.Errorf("store: school offer %s: its key must be the secret it refers to, a %s of tenant %s", o.ID, SecretModelKey, SchoolTenantID)
	}
	return nil
}
