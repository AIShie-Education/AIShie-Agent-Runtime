package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// sealed is a secret as the vault would give it to a store: the bytes are
// stand-ins, since a store neither reads nor checks them.
func sealed(id, tenant, kind string) store.Secret {
	return store.Secret{
		ID: id, TenantID: tenant, Kind: kind, KEKID: "local:v1",
		WrappedDEK: []byte("wrapped-" + id + "\x00\xff"), Nonce: []byte("nonce-" + id), Ciphertext: []byte("ct-" + id + "\x00\x01"),
		Hint: "ais_k7v2m4qhx3ab…", CreatedBy: "0192f3c1-0000-7000-8000-000000000001", CreatedAt: at(time.Minute),
	}
}

func putSecret(t *testing.T, s store.Store, sec store.Secret) {
	t.Helper()
	if err := s.PutSecret(t.Context(), sec); err != nil {
		t.Fatalf("PutSecret(%s): %v", sec.ID, err)
	}
}

func getSecret(t *testing.T, s store.Store, id string) *store.Secret {
	t.Helper()
	got, err := s.Secret(t.Context(), id)
	if err != nil {
		t.Fatalf("Secret(%s): %v", id, err)
	}
	return got
}

// sameSecret compares every field, bytes by value and times to the
// microsecond.
func sameSecret(t *testing.T, got, want store.Secret) {
	t.Helper()
	for _, b := range []struct {
		name      string
		got, want []byte
	}{{"wrapped_dek", got.WrappedDEK, want.WrappedDEK}, {"nonce", got.Nonce, want.Nonce}, {"ciphertext", got.Ciphertext, want.Ciphertext}} {
		if !bytes.Equal(b.got, b.want) {
			t.Errorf("secret %s: %s = %q, want %q", want.ID, b.name, b.got, b.want)
		}
	}
	sameTime(t, "secret "+want.ID+" created_at", got.CreatedAt, want.CreatedAt)
	got.WrappedDEK, got.Nonce, got.Ciphertext, got.CreatedAt = nil, nil, nil, time.Time{}
	want.WrappedDEK, want.Nonce, want.Ciphertext, want.CreatedAt = nil, nil, nil, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secret %s:\n got %+v\nwant %+v", want.ID, got, want)
	}
}

func testSecrets(t *testing.T, open Opener) {
	t.Run("kept byte for byte, every field", func(t *testing.T) {
		s := open(t)
		for _, sec := range []store.Secret{
			sealed("sec_token", "ten_a", store.SecretCoreToken),
			sealed("sec_key", "ten_a", store.SecretModelKey),
		} {
			putSecret(t, s, sec)
			sameSecret(t, *getSecret(t, s, sec.ID), sec)
		}
	})

	t.Run("a zero time is the store's now", func(t *testing.T) {
		s := open(t)
		sec := sealed("sec_now", "ten_a", store.SecretCoreToken)
		sec.CreatedAt = time.Time{}
		before := time.Now()
		putSecret(t, s, sec)
		recent(t, "created_at", getSecret(t, s, sec.ID).CreatedAt, before, time.Now())
	})

	t.Run("an id taken is ErrExists, and the secret there stays", func(t *testing.T) {
		s := open(t)
		first := sealed("sec_1", "ten_a", store.SecretCoreToken)
		putSecret(t, s, first)
		second := sealed("sec_1", "ten_b", store.SecretModelKey)
		second.Ciphertext = []byte("other")
		if err := s.PutSecret(t.Context(), second); !errors.Is(err, store.ErrExists) {
			t.Fatalf("PutSecret of a taken id: err = %v, want ErrExists", err)
		}
		sameSecret(t, *getSecret(t, s, "sec_1"), first)
	})

	t.Run("one not there is ErrNotFound", func(t *testing.T) {
		s := open(t)
		if got, err := s.Secret(t.Context(), "sec_nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Secret = %+v, %v; want ErrNotFound", got, err)
		}
	})

	t.Run("what a caller does to the bytes it gave or got is its own", func(t *testing.T) {
		s := open(t)
		sec := sealed("sec_1", "ten_a", store.SecretCoreToken)
		want := sealed("sec_1", "ten_a", store.SecretCoreToken)
		putSecret(t, s, sec)
		sec.Ciphertext[0], sec.WrappedDEK[0], sec.Nonce[0] = 'X', 'X', 'X'
		got := getSecret(t, s, "sec_1")
		got.Ciphertext[0], got.WrappedDEK[0], got.Nonce[0] = 'Y', 'Y', 'Y'
		sameSecret(t, *getSecret(t, s, "sec_1"), want)
	})

	t.Run("refuses a secret without what it is keyed and sealed by", func(t *testing.T) {
		s := open(t)
		for name, mutate := range map[string]func(*store.Secret){
			"no id":               func(x *store.Secret) { x.ID = "" },
			"an id not sec_":      func(x *store.Secret) { x.ID = "key_1" },
			"an id with /":        func(x *store.Secret) { x.ID = "sec_a/b" },
			"no tenant":           func(x *store.Secret) { x.TenantID = "" },
			"an unknown kind":     func(x *store.Secret) { x.Kind = "password" },
			"no kek id":           func(x *store.Secret) { x.KEKID = "" },
			"no wrapped key":      func(x *store.Secret) { x.WrappedDEK = nil },
			"no nonce":            func(x *store.Secret) { x.Nonce = nil },
			"no ciphertext":       func(x *store.Secret) { x.Ciphertext = nil },
			"an empty kind":       func(x *store.Secret) { x.Kind = "" },
			"an id too long":      func(x *store.Secret) { x.ID = "sec_" + string(bytes.Repeat([]byte("a"), 61)) },
			"an id of sec_ alone": func(x *store.Secret) { x.ID = "sec_" },
		} {
			sec := sealed("sec_1", "ten_a", store.SecretCoreToken)
			mutate(&sec)
			if err := s.PutSecret(t.Context(), sec); err == nil {
				t.Errorf("%s: PutSecret was taken", name)
			}
		}
		if got, err := s.ListSecrets(t.Context(), "", 10); err != nil || len(got) != 0 {
			t.Fatalf("ListSecrets = %d secrets, %v; want none: a refused one was kept", len(got), err)
		}
	})

	t.Run("ListSecrets pages through every secret by id, bytewise", func(t *testing.T) {
		s := open(t)
		ids := []string{"sec_b", "sec_A", "sec_a", "sec_10", "sec_9", "sec_-"}
		for _, id := range ids {
			putSecret(t, s, sealed(id, "ten_"+id, store.SecretModelKey))
		}
		want := slices.Clone(ids)
		slices.Sort(want)
		var got []string
		after := ""
		for page := 0; ; page++ {
			if page > len(ids) {
				t.Fatal("ListSecrets never ends")
			}
			secs, err := s.ListSecrets(t.Context(), after, 4)
			if err != nil {
				t.Fatal(err)
			}
			if len(secs) > 4 {
				t.Fatalf("a page of %d, want at most 4", len(secs))
			}
			if len(secs) == 0 {
				break
			}
			for _, sec := range secs {
				got = append(got, sec.ID)
				if sec.TenantID != "ten_"+sec.ID || len(sec.Ciphertext) == 0 {
					t.Errorf("listed %+v, want its whole row", sec)
				}
			}
			after = secs[len(secs)-1].ID
		}
		if !slices.Equal(got, want) {
			t.Fatalf("ListSecrets pages = %v, want %v", got, want)
		}
		if secs, err := s.ListSecrets(t.Context(), "", 0); err != nil || len(secs) != 0 {
			t.Errorf("ListSecrets with limit 0 = %d, %v; want none", len(secs), err)
		}
	})

	t.Run("RewrapSecret replaces the wrapped key while the old key still wraps it", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		sec := sealed("sec_1", "ten_a", store.SecretCoreToken)
		putSecret(t, s, sec)
		if err := s.RewrapSecret(ctx, "sec_1", "local:v1", "local:v2", []byte("rewrapped")); err != nil {
			t.Fatal(err)
		}
		want := sec
		want.KEKID, want.WrappedDEK = "local:v2", []byte("rewrapped")
		sameSecret(t, *getSecret(t, s, "sec_1"), want)

		// Rewrapped by another since it was read as v1's.
		if err := s.RewrapSecret(ctx, "sec_1", "local:v1", "local:v3", []byte("late")); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a rewrap from a key no longer wrapping it: err = %v, want ErrConflict", err)
		}
		sameSecret(t, *getSecret(t, s, "sec_1"), want)
		if err := s.RewrapSecret(ctx, "sec_nope", "local:v1", "local:v2", []byte("x")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a rewrap of a secret not there: err = %v, want ErrNotFound", err)
		}
		for _, c := range []struct {
			kek     string
			wrapped []byte
		}{{"", []byte("x")}, {"local:v3", nil}} {
			if err := s.RewrapSecret(ctx, "sec_1", "local:v2", c.kek, c.wrapped); err == nil {
				t.Errorf("RewrapSecret to %q, %q was taken", c.kek, c.wrapped)
			}
		}
	})

	t.Run("DeleteSecret destroys one secret; one not there is nothing", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for i := range 2 {
			putSecret(t, s, sealed(fmt.Sprintf("sec_%d", i), "ten_a", store.SecretModelKey))
		}
		if err := s.DeleteSecret(ctx, "sec_0"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSecret(ctx, "sec_0"); err != nil {
			t.Fatalf("deleting it again: %v", err)
		}
		if _, err := s.Secret(ctx, "sec_0"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Secret after DeleteSecret: %v, want ErrNotFound", err)
		}
		getSecret(t, s, "sec_1")
	})
}
