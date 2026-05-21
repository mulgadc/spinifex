package vpcd

// Phase 1 compat layer: the OVN client mock lives in
// spinifex/network/ovn/mock. These aliases keep existing vpcd tests
// compiling while Phase 2 migrates them to import the mock package directly.

import "github.com/mulgadc/spinifex/spinifex/network/ovn/mock"

type MockOVNClient = mock.Client

var NewMockOVNClient = mock.New
