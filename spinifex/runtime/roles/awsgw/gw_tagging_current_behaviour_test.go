//test:in-package — drives EC2_Request through the gateway's unexported test
// helper setupEC2Request.

package awsgw_test

import ()

// The TestCurrentBehaviour_Tagging_ tests here pin today's EC2 tag-write
// outcomes at the AWSGW door. They drive the real router over the real EC2 tag
// service on an embedded JetStream, and describe current behaviour, not the
// target design. The daemon's record-mirror fan-out is not wired here, so only
// the central tag index is exercised.
