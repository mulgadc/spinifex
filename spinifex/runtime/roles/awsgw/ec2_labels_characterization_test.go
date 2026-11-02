// Characterization tests for the labels an EC2 request carries through the
// signed pipeline today: the trace span, the audit action, the throttle key,
// and the service and action the policy gate evaluates. Also the order of the
// cluster-unavailable gate against the checks that follow it. They assert
// current behaviour, not AWS parity.

package awsgw_test

import ()
