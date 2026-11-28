package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Compute Blade LEDs: two addressable LEDs in series on GPIO18 (a yellow one on top, a green one on the
// edge), driven through the Pi 5's RP1 PIO block (see pio_linux.go). CM4 boards have no RP1, so they are
// not supported here yet. Marked experimental in the dashboard until it has been seen working on a blade.

const (
	bladePIODev = "/dev/pio0"
	bladePin    = 18
)

var bladeNames = [2]string{"blade-top", "blade-edge"}

type rgb = [3]byte

// bladeColour is what each pattern shows on a blade LED, and whether it blinks.
func bladeColour(pattern string) (c rgb, blink time.Duration) {
	switch pattern {
	case "slow":
		return rgb{255, 140, 0}, 600 * time.Millisecond // warning: amber
	case "fast":
		return rgb{255, 0, 0}, 120 * time.Millisecond // critical: red
	case "identify":
		return rgb{0, 0, 255}, 120 * time.Millisecond // blue, so it is not mistaken for a warning
	case "on":
		return rgb{0, 160, 0}, 0
	}
	return rgb{}, 0 // restore, off, stealth
}

type bladeLEDs struct {
	open func() (pixelWriter, error)
	mu   sync.Mutex
	w    pixelWriter
	cur  [2]rgb
	stop [2]context.CancelFunc
	err  error
}

func newBladeLEDs(devExists func() bool, open func() (pixelWriter, error)) ledDriver {
	if !devExists() {
		return unsupportedLEDs{note: "No " + bladePIODev + " on this device. Blade LEDs need a Pi 5 or CM5 (their GPIO goes through the RP1 chip's PIO). CM4 blades are not supported yet."}
	}
	return &bladeLEDs{open: open}
}

func (b *bladeLEDs) List() ([]api.LED, string, error) {
	out := make([]api.LED, 2)
	for i, n := range bladeNames {
		out[i] = api.LED{Name: n, Role: "status", Max: 255}
	}
	return out, "", nil
}

func (b *bladeLEDs) index(name string) (int, error) {
	for i, n := range bladeNames {
		if n == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no blade LED called %q (use blade-top or blade-edge)", name)
}

// push writes both pixels. Callers hold mu.
func (b *bladeLEDs) push() error {
	if b.w == nil {
		w, err := b.open()
		if err != nil {
			return err
		}
		b.w = w
	}
	return b.w.Write([][3]byte{b.cur[0], b.cur[1]})
}

func (b *bladeLEDs) Set(name, pattern string) error {
	i, err := b.index(name)
	if err != nil {
		return err
	}
	switch pattern {
	case "restore", "off", "on", "slow", "fast", "identify", "stealth":
	default:
		return fmt.Errorf("unknown LED pattern %q", pattern)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.stop[i]; c != nil {
		c()
		b.stop[i] = nil
	}
	col, blink := bladeColour(pattern)
	b.cur[i] = col
	if err := b.push(); err != nil {
		return err
	}
	if blink > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		b.stop[i] = cancel
		go func() {
			t := time.NewTicker(blink)
			defer t.Stop()
			on := false
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				b.mu.Lock()
				if ctx.Err() == nil {
					if on {
						b.cur[i] = col
					} else {
						b.cur[i] = rgb{}
					}
					b.push()
				}
				b.mu.Unlock()
				on = !on
			}
		}()
	}
	return nil
}

func (b *bladeLEDs) Restore() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.stop {
		if b.stop[i] != nil {
			b.stop[i]()
			b.stop[i] = nil
		}
	}
	if b.w != nil {
		b.cur = [2]rgb{}
		b.push()
		b.w.Close()
		b.w = nil
	}
}

func rp1Opener() (pixelWriter, error) {
	w, err := openRP1PIO(bladePIODev, bladePin)
	if err != nil {
		return nil, errors.New(err.Error())
	}
	return w, nil
}

// BladeLEDTest is the quick hardware check: run `sudo orchestrator-agent -blade-led-test`. It tries the
// PIO setup with several assumed PIO clock speeds (the one thing about the setup that is only a guess),
// each for a few seconds, and says which attempt is running, so the one that lights the LEDs can be named.
func BladeLEDTest(out interface{ Write([]byte) (int, error) }) error {
	say := func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) }
	say("Compute Blade LED test. Watch the top and edge LEDs. Each attempt shows the top LED red and the edge LED blue (dimmed) for 3 seconds, then off.")
	var firstErr error
	for i, hz := range []int{200_000_000, 150_000_000, 125_000_000, 100_000_000} {
		say("")
		say("Attempt %d: assuming a %d MHz PIO clock ...", i+1, hz/1_000_000)
		w, err := openRP1PIOClock(bladePIODev, bladePin, hz)
		if err != nil {
			say("  failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := w.Write([][3]byte{{60, 0, 0}, {0, 0, 60}}); err != nil {
			say("  write failed: %v", err)
			w.Close()
			continue
		}
		time.Sleep(20 * time.Millisecond)
		if empty, err := w.txEmpty(); err != nil {
			say("  FIFO check failed: %v", err)
		} else {
			say("  state machine took the data: %v", empty)
		}
		say("  NOW: top LED red? edge LED blue?")
		time.Sleep(3 * time.Second)
		err = w.Write([][3]byte{{}, {}})
		time.Sleep(20 * time.Millisecond)
		empty, e2 := w.txEmpty()
		say("  sent the off frame: write error=%v, taken=%v (%v)", err, empty, e2)
		time.Sleep(2 * time.Second)
		say("  are both LEDs dark NOW, before the program is closed?")
		time.Sleep(2 * time.Second)
		w.Close()
		say("  off. (3 second pause, both LEDs should be dark)")
		time.Sleep(3 * time.Second)
	}
	say("")
	say("Done. Tell me which attempt (if any) showed red on top and blue on the edge.")
	return firstErr
}
