//go:build !unix

package server

import "errors"

func diskSpace(dir string) (total, free uint64, err error) {
	return 0, 0, errors.New("disk space is not available on this platform")
}

func isNoSpace(err error) bool { return false }
