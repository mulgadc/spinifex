package route53

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	hostedZonePrefix = "/hostedzone/"
	changePrefix     = "/change/"
	idAlphabet       = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	idRandomLength   = 20
)

// newZoneID returns an ID in the shape AWS issues: Z and 20 uppercase
// alphanumerics.
func newZoneID() string { return newID('Z') }

// newChangeID returns a change ID: C and 20 uppercase alphanumerics.
func newChangeID() string { return newID('C') }

func newID(prefix byte) string {
	buf := make([]byte, idRandomLength)
	_, _ = rand.Read(buf)
	out := make([]byte, 0, idRandomLength+1)
	out = append(out, prefix)
	for _, b := range buf {
		out = append(out, idAlphabet[int(b)%len(idAlphabet)])
	}
	return string(out)
}

// BareZoneID strips the "/hostedzone/" prefix, which AWS accepts on input and
// returns on output. The gateway authorizes against the same form.
func BareZoneID(id string) string { return strings.TrimPrefix(strings.TrimSpace(id), hostedZonePrefix) }

// BareChangeID strips the "/change/" prefix, as BareZoneID does for zones.
func BareChangeID(id string) string { return strings.TrimPrefix(strings.TrimSpace(id), changePrefix) }

// refDigest keys a caller reference, which may hold characters a KV key
// cannot.
func refDigest(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:16])
}
