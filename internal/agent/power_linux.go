package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Compute Blade power source. The blade ties GPIO23 to its PoE module: low means PoE+ or PoE++ (up to
// 30 W), high means 5 V only (PoE or USB-C, up to 15 W), per the vendor's PoE guide (whose two code
// samples disagree on the level; the table is followed here). It tells the supply, not the draw.

const powerPin = 23

// gpio uapi v2 (linux/gpio.h)
type gpioConfigAttr struct {
	id, pad uint32
	val     uint64
	mask    uint64
}
type gpioLineConfig struct {
	flags    uint64
	numAttrs uint32
	pad      [5]uint32
	attrs    [10]gpioConfigAttr
}
type gpioLineRequest struct {
	offsets  [64]uint32
	consumer [32]byte
	config   gpioLineConfig
	numLines uint32
	eventBuf uint32
	pad      [5]uint32
	fd       int32
}
type gpioLineValues struct{ bits, mask uint64 }
type gpioChipInfo struct {
	name, label [32]byte
	lines       uint32
}

const (
	gpioFlagInput  = 1 << 2
	gpioFlagPullUp = 1 << 8
)

func ioWR(nr, size uintptr) uintptr { return 3<<30 | size<<16 | 0xb4<<8 | nr }
func ioR(nr, size uintptr) uintptr  { return 2<<30 | size<<16 | 0xb4<<8 | nr }

var (
	reqGPIOChipInfo  = ioR(0x01, unsafe.Sizeof(gpioChipInfo{}))
	reqGPIOGetLine   = ioWR(0x07, unsafe.Sizeof(gpioLineRequest{}))
	reqGPIOGetValues = ioWR(0x0e, unsafe.Sizeof(gpioLineValues{}))
)

// readGPIO reads one line of the GPIO chip whose label is one of labels, with the internal pull-up on.
func readGPIO(labels []string, line uint32) (bool, error) {
	chips, _ := filepath.Glob("/dev/gpiochip*")
	for _, dev := range chips {
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		var info gpioChipInfo
		if _, e := ioctl2(f.Fd(), reqGPIOChipInfo, unsafe.Pointer(&info)); e != nil {
			f.Close()
			continue
		}
		label := strings.TrimRight(string(info.label[:]), "\x00")
		match := false
		for _, l := range labels {
			if label == l {
				match = true
			}
		}
		if !match || info.lines <= line {
			f.Close()
			continue
		}
		var req gpioLineRequest
		req.offsets[0] = line
		copy(req.consumer[:], "orchestrator-agent")
		req.numLines = 1
		req.config.flags = gpioFlagInput | gpioFlagPullUp
		_, e := ioctl2(f.Fd(), reqGPIOGetLine, unsafe.Pointer(&req))
		f.Close()
		if e != nil {
			return false, fmt.Errorf("%s: %w", dev, e)
		}
		lf := os.NewFile(uintptr(req.fd), "gpio-line")
		v := gpioLineValues{mask: 1}
		_, e = ioctl2(lf.Fd(), reqGPIOGetValues, unsafe.Pointer(&v))
		lf.Close() // releasing the line gives the pin back
		if e != nil {
			return false, e
		}
		return v.bits&1 == 1, nil
	}
	return false, fmt.Errorf("no GPIO chip labelled %v", labels)
}

// powerFromLevel turns GPIO23's level into a supply.
func powerFromLevel(high bool) *api.PowerSource {
	if high {
		return &api.PowerSource{Source: api.PowerBasic, MaxWatts: 15}
	}
	return &api.PowerSource{Source: api.PowerPoEPlus, MaxWatts: 30}
}

type powerCache struct {
	mu   sync.Mutex
	at   time.Time
	val  *api.PowerSource
	read func() (bool, error)
}

func (c *powerCache) get() *api.PowerSource {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < time.Minute {
		return c.val
	}
	c.at = time.Now()
	read := c.read
	if read == nil {
		read = func() (bool, error) {
			return readGPIO([]string{"pinctrl-rp1", "pinctrl-bcm2711", "pinctrl-bcm2835", "pinctrl-bcm2712"}, powerPin)
		}
	}
	if high, err := read(); err == nil {
		c.val = powerFromLevel(high)
	} else {
		c.val = nil
	}
	return c.val
}
