package testutil

// HelperT is a subset of testing.T that implements the Helper function
type HelperT interface {
	Helper()
}
