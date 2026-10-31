// Command orchestrator-agent runs on each managed device (Raspberry Pi, VM, server) and
// connects it to an Orchestrator control plane.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Maffewem/Orchestrator/internal/agent"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := agent.Config{}
	var labels string
	flag.StringVar(&cfg.Server, "server", env("ORCH_SERVER", ""), "control plane URL, e.g. http://orchestrator.local:8080")
	flag.StringVar(&cfg.JoinToken, "join-token", env("ORCH_JOIN_TOKEN", ""), "join token from the dashboard (first run only)")
	flag.StringVar(&cfg.Name, "name", env("ORCH_NAME", ""), "device name (defaults to the hostname)")
	flag.StringVar(&labels, "labels", env("ORCH_LABELS", ""), "comma-separated roles/labels, e.g. pi,edge,dns")
	flag.StringVar(&cfg.Setup, "setup", env("ORCH_SETUP", ""), "setup plan ID from the dashboard wizard (first run only)")
	flag.StringVar(&cfg.StatePath, "state", env("ORCH_STATE", "/var/lib/orchestrator-agent/state.json"), "where the agent keeps its identity")
	flag.StringVar(&cfg.DockerHost, "docker-host", env("DOCKER_HOST", "unix:///var/run/docker.sock"), "Docker engine address")
	flag.StringVar(&cfg.HostRoot, "host-root", env("ORCH_HOST_ROOT", ""), "host filesystem mount when running in a container, e.g. /host")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *version {
		os.Stdout.WriteString(agent.Version + "\n")
		return
	}
	for _, l := range strings.Split(labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			cfg.Labels = append(cfg.Labels, l)
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg.Logger = log

	a, err := agent.New(cfg)
	if err != nil {
		log.Error("start agent", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}
