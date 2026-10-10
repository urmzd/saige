// Package must holds the one helper tests in this module use to call a
// constructor that returns an error when the test has no failure to expect.
package must

// Get returns v, and panics when err is not nil. Use it only in tests: a
// panic fails the test with the constructor's error and a stack trace.
func Get[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
