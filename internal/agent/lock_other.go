//go:build !unix

package agent

// LockState is a no-op where file locks are not available.
func LockState(string) (func(), error) { return func() {}, nil }
