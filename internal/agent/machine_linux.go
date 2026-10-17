package agent

import "syscall"

// machine is the kernel's machine name, e.g. "armv7l".
func machine() string {
	var u syscall.Utsname
	if syscall.Uname(&u) != nil {
		return ""
	}
	var m []byte
	for _, c := range u.Machine {
		if c == 0 {
			break
		}
		m = append(m, byte(c))
	}
	return string(m)
}
