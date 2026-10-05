package storage

import (
	"context"
	"testing"
)

func TestCredentialRepositoryUpdateCASListAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := Credential{ID: NewID(), Provider: "google", MaskedSuffix: "4321", ValidationState: "unvalidated", CapabilitiesJSON: "[]", Ciphertext: []byte("cipher-1"), KeyID: "old"}
	if err := s.Credentials().Put(ctx, c); err != nil {
		t.Fatal(err)
	}
	first, err := s.Credentials().Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyID != "old" || string(first.Ciphertext) != "cipher-1" {
		t.Fatalf("stored=%+v", first)
	}
	first.KeyID, first.Ciphertext, first.MaskedSuffix = "new", []byte("cipher-2"), "9876"
	if err := s.Credentials().ReplaceIfUnchanged(ctx, "old", []byte("cipher-1"), first); err != nil {
		t.Fatal(err)
	}
	if err := s.Credentials().ReplaceIfUnchanged(ctx, "old", []byte("cipher-1"), first); err == nil {
		t.Fatal("stale ciphertext replaced a newer credential")
	}
	items, err := s.Credentials().List(ctx)
	if err != nil || len(items) != 1 || items[0].KeyID != "new" || string(items[0].Ciphertext) != "cipher-2" {
		t.Fatalf("credentials=%+v err=%v", items, err)
	}
	if err := s.Credentials().Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Credentials().Get(ctx, c.ID); err == nil {
		t.Fatal("deleted credential remained available")
	}
}

func TestCredentialRotationOnlyChangesEncryptionFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := Credential{ID: NewID(), Provider: "google", MaskedSuffix: "old!", ValidationState: "valid", CapabilitiesJSON: "[\"stt\"]", Ciphertext: []byte("old-cipher"), KeyID: "old"}
	if err := s.Credentials().Put(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.Credentials().RotateIfUnchanged(ctx, c.ID, "old", []byte("old-cipher"), "new", []byte("new-cipher"), "new!"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Credentials().Get(ctx, c.ID)
	if err != nil || got.KeyID != "new" || string(got.Ciphertext) != "new-cipher" || got.MaskedSuffix != "new!" || got.ValidationState != "valid" || got.CapabilitiesJSON != "[\"stt\"]" {
		t.Fatalf("rotation row=%+v err=%v", got, err)
	}
	if err := s.Credentials().RotateIfUnchanged(ctx, c.ID, "old", []byte("old-cipher"), "newer", []byte("newer-cipher"), "x"); err == nil {
		t.Fatal("stale rotation snapshot replaced current key")
	}
}
