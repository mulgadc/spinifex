//go:build !lambdatestlink

package hostlink

import lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"

// Protocol is the ALPN protocol release hosts and guests offer.
const Protocol = lambdav1.ALPNv1
