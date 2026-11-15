//go:build !linux

package agent

func dockerSocketGroup() (int, bool) { return 0, false }
