package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSecretStore_LoadStoreRoundtrip(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "multica-claude-oauth-broker", Namespace: "multica"},
		Data: map[string][]byte{
			"access_token":  []byte("ACCESS_A"),
			"refresh_token": []byte("REFRESH_A"),
			"expires_at":    []byte(now.Format(time.RFC3339)),
		},
	}
	k := fake.NewSimpleClientset(existing)
	store := NewSecretStore(k, "multica", "multica-claude-oauth-broker")

	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.AccessToken != "ACCESS_A" || state.RefreshToken != "REFRESH_A" {
		t.Errorf("Load returned wrong state: %+v", state)
	}
	if !state.ExpiresAt.Equal(now) {
		t.Errorf("ExpiresAt = %v, want %v", state.ExpiresAt, now)
	}

	newState := &TokenState{
		AccessToken:  "ACCESS_B",
		RefreshToken: "REFRESH_B",
		ExpiresAt:    now.Add(time.Hour),
	}
	if err := store.Store(context.Background(), newState); err != nil {
		t.Fatalf("Store: %v", err)
	}
	round, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load roundtrip: %v", err)
	}
	if round.AccessToken != "ACCESS_B" || round.RefreshToken != "REFRESH_B" {
		t.Errorf("roundtrip mismatch: %+v", round)
	}
	if !round.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("roundtrip ExpiresAt = %v", round.ExpiresAt)
	}
}

func TestSecretStore_Load_MissingSecret(t *testing.T) {
	k := fake.NewSimpleClientset()
	store := NewSecretStore(k, "multica", "multica-claude-oauth-broker")
	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestSecretStore_Load_MissingRefreshToken(t *testing.T) {
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"access_token": []byte("A")},
	}
	k := fake.NewSimpleClientset(bad)
	store := NewSecretStore(k, "ns", "s")
	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("expected error for missing refresh_token")
	}
}

func TestSecretStore_Load_BadExpiresAt(t *testing.T) {
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data: map[string][]byte{
			"refresh_token": []byte("R"),
			"expires_at":    []byte("not-a-time"),
		},
	}
	k := fake.NewSimpleClientset(bad)
	store := NewSecretStore(k, "ns", "s")
	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("expected error for unparseable expires_at")
	}
}

func TestSecretStore_Store_CreatesWhenMissing(t *testing.T) {
	k := fake.NewSimpleClientset()
	store := NewSecretStore(k, "ns", "s")
	state := &TokenState{
		AccessToken:  "A",
		RefreshToken: "R",
		ExpiresAt:    time.Now().UTC(),
	}
	if err := store.Store(context.Background(), state); err != nil {
		t.Fatalf("Store on missing: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after Store: %v", err)
	}
	if got.AccessToken != "A" || got.RefreshToken != "R" {
		t.Errorf("created secret missing data: %+v", got)
	}
}
