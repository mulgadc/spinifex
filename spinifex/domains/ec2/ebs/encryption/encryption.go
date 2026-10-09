// Package ebsencryption owns EC2's AWS-visible EBS encryption posture: whether
// a volume the control plane reports is Encrypted. It is EC2's temporary
// reading of provider configuration (the shared Viperblock master-key file),
// not an encryption implementation; the provider encrypts on its own. Its
// intended successor is a providers/ebs capability that reports each volume's
// encryption posture, after which EC2 stops reading key files.
package ebsencryption

import (
	"fmt"
	"sync"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
)

// viperblockKeyCache memoises masterkey.LoadShared by path. The *Key holds an AEAD safe for concurrent use.
var (
	viperblockKeyCacheMu sync.Mutex
	viperblockKeyCache   = map[string]*masterkey.Key{}
)

// Enabled reports whether volumes are encrypted at rest under the configured
// key file. An empty path means encryption is disabled. A configured key that
// cannot be loaded is an error; each caller decides what that means for it.
func Enabled(keyFile string) (bool, error) {
	k, err := loadViperblockMasterKey(keyFile)
	if err != nil {
		return false, err
	}
	return k != nil, nil
}

// loadViperblockMasterKey returns the cached *masterkey.Key for path, loading on first use.
// An empty path returns (nil, nil), meaning encryption is disabled.
func loadViperblockMasterKey(path string) (*masterkey.Key, error) {
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
