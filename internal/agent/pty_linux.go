package agent

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// A pseudo-terminal without a library: open /dev/ptmx, unlock it and open the matching slave.

type winsize struct{ rows, cols, x, y uint16 }

func ioctl(fd uintptr, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// startPTY runs cmd on a new terminal and returns the master side.
func startPTY(cmd *exec.Cmd, cols, rows int) (*os.File, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("no terminal available: %w", err)
	}
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		return nil, err
	}
	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		return nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, err
	}
	defer slave.Close()
	resizePTY(m, cols, rows)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func resizePTY(m *os.File, cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	ws := winsize{rows: uint16(rows), cols: uint16(cols)}
	ioctl(m.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}
