// SPDX-License-Identifier: AGPL-3.0-or-later

// Package taskdir allocates the on-disk directory that holds one task.
package taskdir

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var (
	mu   sync.Mutex
	last int64
)

// New creates <root>/tasks/<id>/ and returns the directory and the id.
//
// Ids carry a millisecond timestamp prefix, so they sort by creation order.
func New(root string) (string, string, error) {
	id, err := newID()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(root, "tasks", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("taskdir: create %s: %w", dir, err)
	}
	return dir, id, nil
}

// newID returns a sortable identifier that is unique within this process.
func newID() (string, error) {
	mu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= last {
		ms = last + 1
	}
	last = ms
	mu.Unlock()

	var suffix [5]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("taskdir: read random bytes: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(suffix[:])
	return strconv.FormatInt(ms, 36) + "-" + enc, nil
}
