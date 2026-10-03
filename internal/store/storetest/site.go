package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// offer is an offer of the school's plan as the API would create it, its
// key sealed as the secret key.
func offer(id, key string) store.SchoolOffer {
	return store.SchoolOffer{
		ID: id, Label: "School AI " + id, Adapter: "openai_chat", Provider: "deepseek", Model: "deepseek-chat",
		BaseURL: "https://api.deepseek.com", MaxOutputTokens: 1200, ReasoningEffort: "low", Enabled: true,
		KeySecretID: key, KeyHint: "sk-…3f9a", KeyTested: true, CreatedBy: "admin-1", CreatedAt: at(2 * time.Hour),
	}
}

// schoolKey is an offer's key, as the vault would seal it.
func schoolKey(id string) store.Secret { return sealed(id, store.SchoolTenantID, store.SecretModelKey) }

func createOffer(t *testing.T, s store.Store, o store.SchoolOffer, key store.Secret) *store.SchoolOffer {
	t.Helper()
	got, err := s.CreateSchoolOffer(t.Context(), o, key)
	if err != nil {
		t.Fatalf("CreateSchoolOffer(%s): %v", o.ID, err)
	}
	return got
}

func getOffer(t *testing.T, s store.Store, id string) *store.SchoolOffer {
	t.Helper()
	o, err := s.SchoolOffer(t.Context(), id)
	if err != nil {
		t.Fatalf("SchoolOffer(%s): %v", id, err)
	}
	return o
}

// sameOffer compares every field, the times only where want has them.
func sameOffer(t *testing.T, got, want store.SchoolOffer) {
	t.Helper()
	if !want.CreatedAt.IsZero() {
		sameTime(t, "offer "+want.ID+" created_at", got.CreatedAt, want.CreatedAt)
	}
	if !want.UpdatedAt.IsZero() {
		sameTime(t, "offer "+want.ID+" updated_at", got.UpdatedAt, want.UpdatedAt)
	}
	got.CreatedAt, want.CreatedAt, got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("offer %s:\n got %+v\nwant %+v", want.ID, got, want)
	}
}

func offerIDs(os []store.SchoolOffer) []string {
	out := make([]string, len(os))
	for i, o := range os {
		out[i] = o.ID
	}
	return out
}

func testSite(t *testing.T, open Opener) {
	t.Run("settings set, replaced, listed by name and unset", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		put := func(st store.SiteSetting) {
			t.Helper()
			if err := s.PutSiteSetting(ctx, st); err != nil {
				t.Fatalf("PutSiteSetting(%s): %v", st.Name, err)
			}
		}
		put(store.SiteSetting{Name: store.SettingSchoolQuotas, Value: json.RawMessage(`{"per_owner_day": 50, "per_asker_day": 10, "per_day": null}`),
			UpdatedBy: "admin-1", UpdatedAt: at(time.Minute)})
		put(store.SiteSetting{Name: store.SettingOCR, Value: json.RawMessage(`{"enabled": true}`), UpdatedBy: "admin-1", UpdatedAt: at(time.Minute)})
		before := time.Now()
		put(store.SiteSetting{Name: store.SettingOCR, Value: json.RawMessage(`{"enabled": false, "languages": ["eng"]}`), UpdatedBy: "admin-2"})
		got, err := s.SiteSettings(ctx)
		if err != nil || len(got) != 2 || got[0].Name != store.SettingOCR || got[1].Name != store.SettingSchoolQuotas {
			t.Fatalf("SiteSettings = %+v, %v", got, err)
		}
		sameJSON(t, "ocr", got[0].Value, json.RawMessage(`{"enabled": false, "languages": ["eng"]}`))
		recent(t, "ocr updated_at", got[0].UpdatedAt, before, time.Now())
		if got[0].UpdatedBy != "admin-2" {
			t.Errorf("ocr updated_by %q", got[0].UpdatedBy)
		}
		sameJSON(t, "school_quotas", got[1].Value, json.RawMessage(`{"per_owner_day": 50, "per_asker_day": 10, "per_day": null}`))
		sameTime(t, "school_quotas updated_at", got[1].UpdatedAt, at(time.Minute))

		if err := s.DeleteSiteSetting(ctx, store.SettingOCR); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSiteSetting(ctx, store.SettingOCR); err != nil {
			t.Errorf("unsetting it again: %v", err)
		}
		if got, err := s.SiteSettings(ctx); err != nil || len(got) != 1 || got[0].Name != store.SettingSchoolQuotas {
			t.Errorf("after unsetting ocr: %+v, %v", got, err)
		}
		for _, bad := range []store.SiteSetting{
			{Name: "", Value: json.RawMessage(`{}`)},
			{Name: "OCR", Value: json.RawMessage(`{}`)},
			{Name: "ocr settings", Value: json.RawMessage(`{}`)},
			{Name: store.SettingOCR},
			{Name: store.SettingOCR, Value: json.RawMessage(`[true]`)},
			{Name: store.SettingOCR, Value: json.RawMessage(`null`)},
			{Name: store.SettingOCR, Value: json.RawMessage(`{"enabled":`)},
		} {
			if err := s.PutSiteSetting(ctx, bad); err == nil {
				t.Errorf("PutSiteSetting(%q, %s) was taken", bad.Name, bad.Value)
			}
		}
		if got, _ := s.SiteSettings(ctx); len(got) != 1 {
			t.Errorf("refused settings were kept: %+v", got)
		}
	})

	t.Run("an offer created with its key, and read back every way", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		o := offer("standard", "sec_school_1")
		got := createOffer(t, s, o, schoolKey("sec_school_1"))
		want := o
		want.Version, want.UpdatedBy, want.UpdatedAt = 1, "admin-1", o.CreatedAt
		sameOffer(t, *got, want)
		sameOffer(t, *getOffer(t, s, "standard"), want)
		sameSecret(t, *getSecret(t, s, "sec_school_1"), schoolKey("sec_school_1"))

		b := offer("basic", "sec_school_2")
		b.Enabled, b.BaseURL, b.Region, b.MaxOutputTokens, b.ReasoningEffort, b.KeyTested, b.CreatedAt = false, "", "", 0, "", false, time.Time{}
		before := time.Now()
		gotB := createOffer(t, s, b, schoolKey("sec_school_2"))
		recent(t, "created_at", gotB.CreatedAt, before, time.Now())
		if !gotB.UpdatedAt.Equal(gotB.CreatedAt) || gotB.Enabled || gotB.KeyTested || gotB.MaxOutputTokens != 0 {
			t.Errorf("an offer turned off, untested, of the defaults: %+v", gotB)
		}
		all, err := s.SchoolOffers(ctx)
		if err != nil || !slices.Equal(offerIDs(all), []string{"basic", "standard"}) {
			t.Fatalf("SchoolOffers = %v, %v", offerIDs(all), err)
		}
		sameOffer(t, all[1], want)
		if _, err := s.SchoolOffer(ctx, "premium"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an offer not there: %v", err)
		}
	})

	t.Run("an offer refused, and nothing of it kept", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		createOffer(t, s, offer("standard", "sec_school_1"), schoolKey("sec_school_1"))
		putSecret(t, s, schoolKey("sec_loose"))
		r := rev(t, s)
		for _, c := range []struct {
			name   string
			o      store.SchoolOffer
			key    store.Secret
			exists bool
		}{
			{"an id taken", offer("standard", "sec_school_2"), schoolKey("sec_school_2"), true},
			{"a key id taken", offer("basic", "sec_loose"), schoolKey("sec_loose"), true},
			{"a key it does not refer to", offer("basic", "sec_school_2"), schoolKey("sec_school_3"), false},
			{"a key of an owner's tenant", offer("basic", "sec_school_2"), sealed("sec_school_2", "ten_owner1", store.SecretModelKey), false},
			{"a Core token as its key", offer("basic", "sec_school_2"), sealed("sec_school_2", store.SchoolTenantID, store.SecretCoreToken), false},
			{"no key", offer("basic", ""), store.Secret{}, false},
			{"an id of no offer's shape", offer("basic plan", "sec_school_2"), schoolKey("sec_school_2"), false},
			{"no id", offer("", "sec_school_2"), schoolKey("sec_school_2"), false},
			{"no label", func() store.SchoolOffer { o := offer("basic", "sec_school_2"); o.Label = " "; return o }(), schoolKey("sec_school_2"), false},
			{"no model", func() store.SchoolOffer { o := offer("basic", "sec_school_2"); o.Model = ""; return o }(), schoolKey("sec_school_2"), false},
			{"no adapter", func() store.SchoolOffer { o := offer("basic", "sec_school_2"); o.Adapter = ""; return o }(), schoolKey("sec_school_2"), false},
			{"a negative output bound", func() store.SchoolOffer { o := offer("basic", "sec_school_2"); o.MaxOutputTokens = -1; return o }(),
				schoolKey("sec_school_2"), false},
		} {
			_, err := s.CreateSchoolOffer(ctx, c.o, c.key)
			switch {
			case err == nil:
				t.Errorf("%s: created", c.name)
			case c.exists && !errors.Is(err, store.ErrExists):
				t.Errorf("%s: err = %v, want ErrExists", c.name, err)
			}
			if c.key.ID != "" && c.key.ID != "sec_loose" {
				missingSecret(t, s, c.key.ID)
			}
		}
		if got := rev(t, s); got != r {
			t.Errorf("refused offers moved the revision from %d to %d", r, got)
		}
		all, err := s.SchoolOffers(ctx)
		if err != nil || !slices.Equal(offerIDs(all), []string{"standard"}) {
			t.Errorf("SchoolOffers = %v, %v; want only the first", offerIDs(all), err)
		}
		getSecret(t, s, "sec_school_1")
	})

	t.Run("an offer updated at the version named, or at any", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := *createOffer(t, s, offer("standard", "sec_school_1"), schoolKey("sec_school_1"))
		b := a
		b.Label, b.Model, b.Enabled, b.KeyTested, b.UpdatedBy = "School AI (fast)", "deepseek-reasoner", false, false, "admin-2"
		before := time.Now()
		got, err := s.UpdateSchoolOffer(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		want := b
		want.Version, want.CreatedBy, want.UpdatedAt = 2, "admin-1", time.Time{}
		sameOffer(t, *got, want)
		recent(t, "updated_at", got.UpdatedAt, before, time.Now())
		sameTime(t, "created_at", got.CreatedAt, a.CreatedAt)
		sameOffer(t, *getOffer(t, s, "standard"), want)

		stale := a
		stale.Label = "Stale"
		if _, err := s.UpdateSchoolOffer(ctx, stale); !errors.Is(err, store.ErrConflict) {
			t.Errorf("an update at version 1 of an offer at 2: %v, want ErrConflict", err)
		}
		anyVersion := b
		anyVersion.Version, anyVersion.Label = 0, "School AI"
		if got, err := s.UpdateSchoolOffer(ctx, anyVersion); err != nil || got.Version != 3 || got.Label != "School AI" {
			t.Errorf("an update at no version named: %+v, %v", got, err)
		}
		gone := b
		gone.ID, gone.Version = "premium", 0
		if _, err := s.UpdateSchoolOffer(ctx, gone); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an update of an offer not there: %v", err)
		}
		bad := *getOffer(t, s, "standard")
		bad.Label = ""
		if _, err := s.UpdateSchoolOffer(ctx, bad); err == nil {
			t.Error("an update with no label was taken")
		}
		if got := getOffer(t, s, "standard"); got.Version != 3 {
			t.Errorf("refused updates moved the version to %d", got.Version)
		}
	})

	t.Run("an offer's key replaced destroys the one before, in the same write", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := *createOffer(t, s, offer("standard", "sec_school_1"), schoolKey("sec_school_1"))
		a.KeySecretID, a.KeyHint = "sec_school_2", "sk-…bbbb"
		got, err := s.UpdateSchoolOffer(ctx, a, schoolKey("sec_school_2"))
		if err != nil {
			t.Fatal(err)
		}
		if got.KeySecretID != "sec_school_2" || got.KeyHint != "sk-…bbbb" || got.Version != 2 {
			t.Errorf("after the replacement: %+v", got)
		}
		missingSecret(t, s, "sec_school_1")
		getSecret(t, s, "sec_school_2")
		// A key it refers to is not destroyed on its own.
		if err := s.DeleteSecret(ctx, "sec_school_2"); !errors.Is(err, store.ErrInUse) {
			t.Errorf("DeleteSecret of an offer's key: %v, want ErrInUse", err)
		}
		// A refused replacement keeps the old key and stores nothing.
		for name, key := range map[string]store.Secret{
			"of an owner's tenant": sealed("sec_school_3", "ten_owner1", store.SecretModelKey),
			"not given":            {},
		} {
			bad := *got
			bad.KeySecretID = "sec_school_3"
			var err error
			if key.ID == "" {
				_, err = s.UpdateSchoolOffer(ctx, bad)
			} else {
				_, err = s.UpdateSchoolOffer(ctx, bad, key)
			}
			if err == nil {
				t.Errorf("a key %s was taken", name)
			}
			missingSecret(t, s, "sec_school_3")
		}
		if o := getOffer(t, s, "standard"); o.KeySecretID != "sec_school_2" || o.Version != 2 {
			t.Errorf("refused replacements wrote: %+v", o)
		}
		getSecret(t, s, "sec_school_2")
	})

	t.Run("an offer deleted with its key, at the version named", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		createOffer(t, s, offer("standard", "sec_school_1"), schoolKey("sec_school_1"))
		createOffer(t, s, offer("basic", "sec_school_2"), schoolKey("sec_school_2"))
		last := rev(t, s)
		if err := s.DeleteSchoolOffer(ctx, "standard", 2); !errors.Is(err, store.ErrConflict) {
			t.Errorf("at a version it is not at: %v, want ErrConflict", err)
		}
		getSecret(t, s, "sec_school_1")
		if r := rev(t, s); r != last {
			t.Errorf("a refused delete moved the revision from %d to %d", last, r)
		}
		if err := s.DeleteSchoolOffer(ctx, "standard", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SchoolOffer(ctx, "standard"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the offer after its deletion: %v", err)
		}
		missingSecret(t, s, "sec_school_1")
		if err := s.DeleteSchoolOffer(ctx, "basic", 0); err != nil {
			t.Fatalf("at any version: %v", err)
		}
		missingSecret(t, s, "sec_school_2")
		last = rev(t, s)
		if err := s.DeleteSchoolOffer(ctx, "standard", 0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting it again: %v, want ErrNotFound", err)
		}
		if r := rev(t, s); r != last {
			t.Errorf("deleting an offer not there moved the revision from %d to %d", last, r)
		}
		// Its id may be taken again.
		createOffer(t, s, offer("standard", "sec_school_3"), schoolKey("sec_school_3"))
	})

	t.Run("an offer of OpenRouter's keeps its upstream routing, canonical, and is given as a copy", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		deny, price := "deny", "01.50"
		o := offer("llama", "sec_school_1")
		o.Provider, o.Model, o.BaseURL = "openrouter", "meta-llama/llama-3.3-70b-instruct", "https://openrouter.ai/api/v1"
		o.OpenRouter = &openrouter.Routing{DataCollection: &deny, Only: []string{"groq", "together"}, Ignore: []string{},
			Sort: &openrouter.Sort{By: "price"}, MaxPrice: &openrouter.MaxPrice{Prompt: &price}}
		const want = `{"data_collection":"deny","only":["groq","together"],"sort":"price","max_price":{"prompt":"1.5"}}`
		routingOf := func(what string, got *store.SchoolOffer, want string) {
			t.Helper()
			if r := string(got.OpenRouter.JSON()); r != want {
				t.Errorf("%s: %s, want %s", what, r, want)
			}
			if got.OpenRouter != nil && got.OpenRouter.Ignore != nil {
				t.Errorf("%s: not canonical: %+v", what, got.OpenRouter)
			}
		}
		created := createOffer(t, s, o, schoolKey("sec_school_1"))
		routingOf("created", created, want)
		read := getOffer(t, s, "llama")
		routingOf("read", read, want)
		read.OpenRouter.Only[0] = "novita"
		*read.OpenRouter.MaxPrice.Prompt = "9"
		all, err := s.SchoolOffers(ctx)
		if err != nil || len(all) != 1 {
			t.Fatalf("SchoolOffers = %v, %v", offerIDs(all), err)
		}
		routingOf("listed, after a read was changed", &all[0], want)
		if o.OpenRouter.Only[0] != "groq" {
			t.Error("the store changed the routing it was given")
		}

		u := *getOffer(t, s, "llama")
		u.OpenRouter = &openrouter.Routing{ZDR: new(bool)}
		updated, err := s.UpdateSchoolOffer(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		routingOf("replaced", updated, `{"zdr":false}`)
		routingOf("replaced, read", getOffer(t, s, "llama"), `{"zdr":false}`)
		u = *updated
		u.OpenRouter = &openrouter.Routing{Only: []string{}}
		if updated, err = s.UpdateSchoolOffer(ctx, u); err != nil || updated.OpenRouter != nil {
			t.Fatalf("an empty routing: %v, %v", updated, err)
		}
		if got := getOffer(t, s, "llama"); got.OpenRouter != nil {
			t.Errorf("removed, read: %s", got.OpenRouter.JSON())
		}

		// Routing is OpenRouter's alone.
		d := offer("deepseek", "sec_school_2")
		d.OpenRouter = &openrouter.Routing{ZDR: new(bool)}
		if _, err := s.CreateSchoolOffer(ctx, d, schoolKey("sec_school_2")); err == nil {
			t.Error("routing on an offer of DeepSeek's was taken")
		}
		u = *getOffer(t, s, "llama")
		u.Provider, u.OpenRouter = "deepseek", &openrouter.Routing{ZDR: new(bool)}
		if _, err := s.UpdateSchoolOffer(ctx, u); err == nil {
			t.Error("routing on an offer moved to DeepSeek was taken")
		}
	})

	t.Run("the revision moves on with every write to a setting or an offer", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		last := rev(t, s)
		moved := func(what string) {
			t.Helper()
			if r := rev(t, s); r <= last {
				t.Errorf("%s: the revision is %d, was %d", what, r, last)
			} else {
				last = r
			}
		}
		still := func(what string) {
			t.Helper()
			if r := rev(t, s); r != last {
				t.Errorf("%s: the revision moved from %d to %d", what, last, r)
				last = r
			}
		}
		if err := s.PutSiteSetting(ctx, store.SiteSetting{Name: store.SettingOCR, Value: json.RawMessage(`{"enabled": false}`)}); err != nil {
			t.Fatal(err)
		}
		moved("a setting set")
		if err := s.DeleteSiteSetting(ctx, store.SettingOCR); err != nil {
			t.Fatal(err)
		}
		moved("a setting unset")
		if err := s.DeleteSiteSetting(ctx, store.SettingOCR); err != nil {
			t.Fatal(err)
		}
		still("a setting unset that was not set")
		o := *createOffer(t, s, offer("standard", "sec_school_1"), schoolKey("sec_school_1"))
		moved("an offer created")
		o.Enabled = false
		if _, err := s.UpdateSchoolOffer(ctx, o); err != nil {
			t.Fatal(err)
		}
		moved("an offer turned off")
		if _, err := s.UpdateSchoolOffer(ctx, o); !errors.Is(err, store.ErrConflict) {
			t.Fatal(err)
		}
		still("a refused update")
		if _, err := s.SiteSettings(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SchoolOffers(ctx); err != nil {
			t.Fatal(err)
		}
		still("reads")
		if err := s.DeleteSchoolOffer(ctx, "standard", 0); err != nil {
			t.Fatal(err)
		}
		moved("an offer deleted")
	})
}
