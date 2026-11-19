package ovn

// SetAddressSetInsertHook installs f between CreateAddressSetIfAbsent's cache
// miss and its insert until the test ends.
func SetAddressSetInsertHook(t interface{ Cleanup(func()) }, f func()) {
	addressSetInsertHook = f
	t.Cleanup(func() { addressSetInsertHook = nil })
}
