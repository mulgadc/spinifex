//test:in-package — an export_test.go is in-package by definition; that is the
//whole of what it does. The tests using this live in objectstore_test.

package objectstore

// MaxListPages exposes maxListPages for the external test package, which
// needs it to assert ListAll gives up after exactly the documented budget.
const MaxListPages = maxListPages
