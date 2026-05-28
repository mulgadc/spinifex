// Copyright 2026 Mulga Defense Corporation (MDC). All rights reserved.
// Use of this source code is governed by an Apache 2.0 license
// that can be found in the LICENSE file.

package utils

import (
	"fmt"
	"sync"

	"github.com/mulgadc/predastore/pkg/masterkey"
)

// viperblockKeyCache memoises masterkey.LoadShared by path so the AWS gateway
// handlers (volume / instance / image) do not re-stat and re-parse the same
// 32-byte file on every VB.New. Process-wide; the underlying *Key holds an
// AEAD which is safe for concurrent use per the crypto/cipher contract.
var (
	viperblockKeyCacheMu sync.Mutex
	viperblockKeyCache   = map[string]*masterkey.Key{}
)

// LoadViperblockMasterKey returns the cached *masterkey.Key for the given
// path, loading it via masterkey.LoadShared on first use. An empty path
// returns (nil, nil) — encryption is treated as disabled.
func LoadViperblockMasterKey(path string) (*masterkey.Key, error) {
	if path == "" {
		return nil, nil
	}
	viperblockKeyCacheMu.Lock()
	defer viperblockKeyCacheMu.Unlock()
	if k, ok := viperblockKeyCache[path]; ok {
		return k, nil
	}
	k, err := masterkey.LoadShared(path)
	if err != nil {
		return nil, fmt.Errorf("load viperblock encryption key %s: %w", path, err)
	}
	viperblockKeyCache[path] = k
	return k, nil
}
