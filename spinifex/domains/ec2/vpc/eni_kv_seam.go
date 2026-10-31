package vpc

import "github.com/nats-io/nats.go/jetstream"

// WrapENIKVForTest replaces the ENI bucket handle with wrap(current), so a test
// can inject the read and write faults a replicated bucket produces.
func (s *VPCServiceImpl) WrapENIKVForTest(wrap func(jetstream.KeyValue) jetstream.KeyValue) {
	s.eniKV = wrap(s.eniKV)
}
