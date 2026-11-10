// Package id generates short, time-sortable identifiers.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// New returns a 24-char hex id whose lexical order follows creation time.
func New() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%012x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// Token returns a random secret of n bytes, hex encoded.
func Token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
