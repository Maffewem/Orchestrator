//go:build !unix

package server

import "errors"

func freeBytes(dir string) (uint64, error) {
	return 0, errors.New("free space is not available on this platform")
}

func isNoSpace(err error) bool { return false }
