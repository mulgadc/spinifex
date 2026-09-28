package handlers_ec2_vpc

import "context"

// In-package fixtures the external test package needs.
const TestAccountID = testAccountID

var (
	SetupTestVPCService = setupTestVPCService
	CreateTestVPC       = createTestVPC
	CreateTestSubnet    = createTestSubnet
)

// FakeCentralTagStore records the tag map passed to PutResourceTags per
// resource and the ids passed to DeleteAllTags, and can be made to fail so
// tests can prove a central-store error never fails the operation it tracks.
type FakeCentralTagStore struct {
	Calls   map[string]map[string]string
	Deleted []string
	Err     error
}

var _ CentralTagStore = (*FakeCentralTagStore)(nil)

func (f *FakeCentralTagStore) PutResourceTags(_ context.Context, _, resourceID string, tags map[string]string) error {
	if f.Calls == nil {
		f.Calls = map[string]map[string]string{}
	}
	f.Calls[resourceID] = tags
	return f.Err
}

func (f *FakeCentralTagStore) DeleteAllTags(_ context.Context, _, resourceID string) error {
	f.Deleted = append(f.Deleted, resourceID)
	return f.Err
}
