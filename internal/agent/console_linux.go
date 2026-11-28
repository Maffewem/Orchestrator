package agent

import (
	"context"
	"errors"
	"github.com/Maffewem/Orchestrator/internal/api"
	"os"
	"os/exec"
	"sync"
)

// ptySession is a login shell on the device itself, on a terminal of its own.
type ptySession struct {
	m    *os.File
	cmd  *exec.Cmd
	once sync.Once
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.m.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.m.Write(b) }
func (p *ptySession) Resize(_ context.Context, cols, rows int) error {
	resizePTY(p.m, cols, rows)
	return nil
}
func (p *ptySession) Close() error {
	p.once.Do(func() {
		p.m.Close() // the shell gets a hangup
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		go p.cmd.Wait()
	})
	return nil
}

// serveConsole opens a shell on the device itself (not in a container) and serves it as a stream.
// The agent runs as root, so this is a root shell: the control plane only opens it for administrators
// and records it.
func (a *Agent) serveConsole(ctx context.Context, req api.StreamRequest) error {
	if InContainer() {
		return errors.New("this agent runs in a container, so it has no shell on the device itself; install it as a service on the device to use the console")
	}
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = "/root"
	if _, err := os.Stat(cmd.Dir); err != nil {
		cmd.Dir = "/"
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "HOME=/root", "SHELL="+shell)
	m, err := startPTY(cmd, req.Cols, req.Rows)
	if err != nil {
		return err
	}
	return a.pumpTerminal(ctx, req, &ptySession{m: m, cmd: cmd})
}
