package agent

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// The Raspberry Pi 5 (and CM5) routes its GPIO through the RP1 chip, whose programmable I/O (PIO) block
// is exposed by the kernel as /dev/pio0. This file drives one WS2812-style LED chain from it: the PIO
// state machine makes the 800 kHz data signal, the agent only feeds it colours.
//
// The ioctl layout comes from the kernel's include/uapi/misc/rp1_pio_if.h. What is not from there, and
// so is the part to check on real hardware: the order of calls (modelled on the Pico SDK's WS2812 setup),
// that the PIO runs from a 200 MHz clock, and that ADD_PROGRAM at origin 0 succeeds.

// pixelWriter pushes colours (green, red, blue per pixel) to an LED chain.
type pixelWriter interface {
	Write(px [][3]byte) error // each entry is {r, g, b}
	Close() error
}

const (
	pioMagic     = 102
	rp1FuncPIO   = 7
	pioClockHz   = 200_000_000
	ws2812Hz     = 800_000
	ws2812Cycles = 10 // T1+T2+T3 in the program below
)

// ws2812Program is the Pico SDK's WS2812 program (one side-set pin, 10 cycles per bit):
//
//	.wrap_target
//	bitloop: out x, 1      side 0 [2]
//	         jmp !x, zero  side 1 [1]
//	         jmp bitloop   side 1 [4]
//	zero:    nop           side 0 [4]
//	.wrap
var ws2812Program = [4]uint16{0x6221, 0x1123, 0x1400, 0xa442}

type pioSMConfig struct{ clkdiv, execctrl, shiftctrl, pinctrl uint32 }
type pioAddProgram struct {
	numInstrs, origin uint16
	instrs            [32]uint16
}
type pioRemoveProgram struct{ numInstrs, origin uint16 }
type pioClaim struct{ mask uint16 }
type pioSMInit struct {
	sm, initialPC uint16
	config        pioSMConfig
}
type pioSetPindirs struct {
	sm, rsvd   uint16
	dirs, mask uint32
}
type pioSetEnabled struct {
	mask   uint16
	enable uint8
	rsvd   uint8
}
type pioPut struct {
	sm       uint16
	blocking uint8
	rsvd     uint8
	data     uint32
}
type pioClearFifos struct{ sm uint16 }
type pioGPIOInit struct{ gpio uint16 }
type pioGPIOFunc struct{ gpio, fn uint16 }

// ioW is the kernel's _IOW(type, nr, size) for Linux on arm, arm64 and amd64.
func ioW(nr, size uintptr) uintptr { return 1<<30 | size<<16 | pioMagic<<8 | nr }

var (
	reqAddProgram    = ioW(11, unsafe.Sizeof(pioAddProgram{}))
	reqRemoveProgram = ioW(12, unsafe.Sizeof(pioRemoveProgram{}))
	reqSMClaim       = ioW(20, unsafe.Sizeof(pioClaim{}))
	reqSMUnclaim     = ioW(21, unsafe.Sizeof(pioClaim{}))
	reqSMInit        = ioW(30, unsafe.Sizeof(pioSMInit{}))
	reqSMClearFifos  = ioW(33, unsafe.Sizeof(pioClearFifos{}))
	reqSMSetPindirs  = ioW(36, unsafe.Sizeof(pioSetPindirs{}))
	reqSMSetEnabled  = ioW(37, unsafe.Sizeof(pioSetEnabled{}))
	reqSMPut         = ioW(41, unsafe.Sizeof(pioPut{}))
	reqGPIOInit      = ioW(50, unsafe.Sizeof(pioGPIOInit{}))
	reqGPIOFunction  = ioW(51, unsafe.Sizeof(pioGPIOFunc{}))
)

// ws2812Config is the state machine configuration the Pico SDK's ws2812_program_init builds:
// one side-set pin, autopull at 24 bits shifting left, joined TX FIFO, 800 kHz bit rate.
func ws2812Config(pin, origin uint32, clockHz int) pioSMConfig {
	div := uint32(clockHz / (ws2812Hz * ws2812Cycles)) // whole divider; 200 MHz gives 25
	return pioSMConfig{
		clkdiv:    div << 16,
		execctrl:  (origin+uint32(len(ws2812Program))-1)<<12 | origin<<7, // wrap top, wrap bottom
		shiftctrl: 1<<17 | 24<<25 | 1<<30,                                // autopull, pull threshold 24, out shifts left, join TX
		pinctrl:   1<<29 | pin<<10,                                       // one side-set pin
	}
}

// ws2812Word packs a pixel for the FIFO: red, green, blue in the top 24 bits, so red goes out first.
// Most WS2812 strips want green first, but the Compute Blade's LEDs showed green when sent red through
// a green-first library, so they are red-first.
func ws2812Word(r, g, b byte) uint32 {
	return (uint32(r)<<16 | uint32(g)<<8 | uint32(b)) << 8
}

type rp1PIO struct {
	f      *os.File
	sm     uint16
	origin uint16
	pin    uint16
}

func ioctl2(fd uintptr, req uintptr, arg unsafe.Pointer) (int, error) {
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

// openRP1PIO sets up a state machine that drives pin as a WS2812 chain.
func openRP1PIO(dev string, pin int) (_ *rp1PIO, err error) {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s (needs a Pi 5 or CM5 with the RP1 PIO driver, and root): %w", dev, err)
	}
	p := &rp1PIO{f: f, sm: 0xffff, pin: uint16(pin)}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	fd := f.Fd()
	for i := uint16(0); i < 4 && p.sm == 0xffff; i++ {
		c := pioClaim{mask: 1 << i}
		if _, e := ioctl2(fd, reqSMClaim, unsafe.Pointer(&c)); e == nil {
			p.sm = i
		}
	}
	if p.sm == 0xffff {
		return nil, errors.New("no free PIO state machine (is another program using the PIO?)")
	}
	prog := pioAddProgram{numInstrs: uint16(len(ws2812Program)), origin: 0}
	copy(prog.instrs[:], ws2812Program[:])
	if _, e := ioctl2(fd, reqAddProgram, unsafe.Pointer(&prog)); e != nil {
		return nil, fmt.Errorf("loading the WS2812 program: %w", e)
	}
	p.origin = 0
	gi := pioGPIOInit{gpio: p.pin}
	if _, e := ioctl2(fd, reqGPIOInit, unsafe.Pointer(&gi)); e != nil {
		return nil, fmt.Errorf("gpio init: %w", e)
	}
	gf := pioGPIOFunc{gpio: p.pin, fn: rp1FuncPIO}
	if _, e := ioctl2(fd, reqGPIOFunction, unsafe.Pointer(&gf)); e != nil {
		return nil, fmt.Errorf("gpio function: %w", e)
	}
	init := pioSMInit{sm: p.sm, initialPC: p.origin, config: ws2812Config(uint32(pin), uint32(p.origin), pioClockHz)}
	if _, e := ioctl2(fd, reqSMInit, unsafe.Pointer(&init)); e != nil {
		return nil, fmt.Errorf("state machine init: %w", e)
	}
	pd := pioSetPindirs{sm: p.sm, dirs: 1 << uint(pin), mask: 1 << uint(pin)}
	if _, e := ioctl2(fd, reqSMSetPindirs, unsafe.Pointer(&pd)); e != nil {
		return nil, fmt.Errorf("pin direction: %w", e)
	}
	en := pioSetEnabled{mask: 1 << p.sm, enable: 1}
	if _, e := ioctl2(fd, reqSMSetEnabled, unsafe.Pointer(&en)); e != nil {
		return nil, fmt.Errorf("enabling the state machine: %w", e)
	}
	return p, nil
}

func (p *rp1PIO) Write(px [][3]byte) error {
	for _, c := range px {
		put := pioPut{sm: p.sm, blocking: 1, data: ws2812Word(c[0], c[1], c[2])}
		if _, e := ioctl2(p.f.Fd(), reqSMPut, unsafe.Pointer(&put)); e != nil {
			return e
		}
	}
	return nil
}

// Close switches the LEDs off and gives everything back.
func (p *rp1PIO) Close() error {
	fd := p.f.Fd()
	if p.sm != 0xffff {
		p.Write([][3]byte{{}, {}})
		time.Sleep(5 * time.Millisecond) // let the last bits leave the FIFO
		en := pioSetEnabled{mask: 1 << p.sm}
		ioctl2(fd, reqSMSetEnabled, unsafe.Pointer(&en))
		rm := pioRemoveProgram{numInstrs: uint16(len(ws2812Program)), origin: p.origin}
		ioctl2(fd, reqRemoveProgram, unsafe.Pointer(&rm))
		c := pioClaim{mask: 1 << p.sm}
		ioctl2(fd, reqSMUnclaim, unsafe.Pointer(&c))
	}
	return p.f.Close()
}
