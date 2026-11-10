package main

import (
	"errors"

	"github.com/zalando/go-keyring"
)

// keychain stores secrets in the operating system's credential store
// (Keychain on macOS, Credential Manager on Windows, Secret Service on Linux).
type keychain struct{ service string }

func (k keychain) Get(key string) (string, bool) {
	v, err := keyring.Get(k.service, key)
	return v, err == nil
}

func (k keychain) Set(key, value string) error { return keyring.Set(k.service, key, value) }

func (k keychain) Delete(key string) error {
	if err := keyring.Delete(k.service, key); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// usable checks the credential store works here (a Linux box without a
// Secret Service does not have one), so the app can fall back to a file.
func (k keychain) usable() bool {
	const probe = "cat-probe"
	if err := keyring.Set(k.service, probe, "1"); err != nil {
		return false
	}
	_ = keyring.Delete(k.service, probe)
	return true
}
