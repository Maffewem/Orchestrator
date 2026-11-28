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

// BladeLEDTest is the quick hardware check: run `sudo orchestrator-agent -blade-led-test`. It sets up the
// PIO once at 200 MHz and steps through several frames, naming each, so what the LEDs actually do with
// each one can be reported back.
func BladeLEDTest(out interface{ Write([]byte) (int, error) }) error {
	say := func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) }
	say("Compute Blade LED test. Watch the top and edge LEDs and note what each step shows.")
	steps := []struct {
		name string
		px   [][3]byte
	}{
		{"1: top RED, edge BLUE", [][3]byte{{60, 0, 0}, {0, 0, 60}}},
		{"2: top BLUE, edge RED (swapped)", [][3]byte{{0, 0, 60}, {60, 0, 0}}},
		{"3: both GREEN", [][3]byte{{0, 60, 0}, {0, 60, 0}}},
		{"4: both OFF (all zero)", [][3]byte{{}, {}}},
		{"5: both very dim white (1,1,1)", [][3]byte{{1, 1, 1}, {1, 1, 1}}},
		{"6: both OFF again", [][3]byte{{}, {}}},
		{"7: top RED only, edge off", [][3]byte{{60, 0, 0}, {}}},
		{"8: both OFF, sent three times", [][3]byte{{}, {}}},
	}
	for i, st := range steps {
		say("")
		say("Step %s  (hold 4 seconds)", st.name)
		n := 1
		if i == len(steps)-1 {
			n = 3
		}
		for k := 0; k < n; k++ {
			// A fresh PIO setup for every frame: the earlier test showed only the first frame after a
			// setup reaching the LEDs.
			w, err := openRP1PIOClock(bladePIODev, bladePin, 200_000_000)
			if err != nil {
				say("  failed: %v", err)
				return err
			}
			if err := w.Write(st.px); err != nil {
				say("  write failed: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
			w.closeKeep()
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(4 * time.Second)
	}
	say("")
	say("Done. Tell me what each step looked like (same / changed / which colours).")
	return nil
}
