package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// BrokerState mirrors the three keys the broker writes into its state Secret.
type BrokerState struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time // zero when the key is absent
}

// LoadClusterClient reads kubeconfig the same way kubectl does (KUBECONFIG env,
// then ~/.kube/config). When contextName is non-empty it overrides the current
// context.
func LoadClusterClient(contextName string) (kubernetes.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	restCfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(restCfg)
}

// ReadBrokerState fetches the broker's state Secret and decodes the three keys
// the sync needs. A missing access_token or refresh_token is an error: the
// broker has not finished bootstrapping yet and the local Keychain must not be
// overwritten with a half-populated payload.
func ReadBrokerState(ctx context.Context, k kubernetes.Interface, namespace, name string) (*BrokerState, error) {
	sec, err := k.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get secret %s/%s: %w", namespace, name, err)
	}
	state := &BrokerState{
		AccessToken:  string(sec.Data["access_token"]),
		RefreshToken: string(sec.Data["refresh_token"]),
	}
	if rawExp, ok := sec.Data["expires_at"]; ok && len(rawExp) > 0 {
		t, err := time.Parse(time.RFC3339, string(rawExp))
		if err != nil {
			return nil, fmt.Errorf("parse expires_at %q: %w", rawExp, err)
		}
		state.ExpiresAt = t
	}
	if state.AccessToken == "" || state.RefreshToken == "" {
		return nil, fmt.Errorf("secret %s/%s missing access_token or refresh_token (broker may not have reloaded yet)", namespace, name)
	}
	return state, nil
}

// WriteBrokerState overwrites the broker's source-of-truth Secret with a
// fresh token pair discovered client-side (i.e. the Claude Code CLI on this
// Mac refreshed while the k8s broker was stuck). The broker's next tick
// runs observeAndCheckReseed against the new refresh_token, treats it as an
// operator reseed, and exchanges it — self-healing the loop.
//
// Uses a strategic-merge patch so we never touch other keys the broker (or
// operators) may have annotated on the Secret. Format matches SecretStore.Store
// so downstream reads see identical byte-for-byte state.
func WriteBrokerState(ctx context.Context, k kubernetes.Interface, namespace, name string, state *BrokerState) error {
	if state == nil || state.AccessToken == "" || state.RefreshToken == "" {
		return fmt.Errorf("refuse to write empty broker state")
	}
	// JSON-merge patch against `.data` (base64) rather than `.stringData`.
	// Two reasons: (1) `stringData` is a write-only convenience the API
	// server converts to `.data` — kubernetes.Interface fake clientsets in
	// tests do NOT perform that conversion, so tests reading `.data` back
	// see the pre-patch value; (2) `.data` is a JSON object of base64
	// strings, which JSON-merge patch handles correctly (unlike strategic
	// merge, which for Secret's `data` field is defined as a full replace).
	patch := map[string]any{
		"data": map[string]string{
			"access_token":  base64.StdEncoding.EncodeToString([]byte(state.AccessToken)),
			"refresh_token": base64.StdEncoding.EncodeToString([]byte(state.RefreshToken)),
			"expires_at":    base64.StdEncoding.EncodeToString([]byte(state.ExpiresAt.UTC().Format(time.RFC3339))),
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	_, err = k.CoreV1().Secrets(namespace).Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil && k8serrors.IsNotFound(err) {
		// The broker's source Secret is expected to already exist (bootstrap
		// creates it). If somehow missing, create it — better than failing loud
		// and leaving the fresh refresh_token unpersisted.
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data: map[string][]byte{
				"access_token":  []byte(state.AccessToken),
				"refresh_token": []byte(state.RefreshToken),
				"expires_at":    []byte(state.ExpiresAt.UTC().Format(time.RFC3339)),
			},
		}
		_, err = k.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("write broker state %s/%s: %w", namespace, name, err)
	}
	return nil
}
