// Command orchestrator-server runs the Orchestrator control plane and dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Maffewem/Orchestrator/internal/agent"
	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
	"github.com/Maffewem/Orchestrator/internal/selfupdate"
	"github.com/Maffewem/Orchestrator/internal/server"
	"github.com/Maffewem/Orchestrator/internal/store"
	"github.com/Maffewem/Orchestrator/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "self-update" {
		os.Exit(runSelfUpdate(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "decrypt-backup" {
		os.Exit(runDecryptBackup(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "import-backup" {
		os.Exit(runImportBackup(os.Args[2:]))
	}
	addr := flag.String("addr", env("ORCH_ADDR", ":8080"), "listen address")
	dbPath := flag.String("db", env("ORCH_DB", "orchestrator.db"), "SQLite database path")
	downloads := flag.String("downloads", env("ORCH_DOWNLOADS", ""), "directory of agent binaries served to /install.sh")
	interval := flag.Duration("heartbeat", 5*time.Second, "agent heartbeat interval")
	backups := flag.String("backups", env("ORCH_BACKUPS", ""), "directory for container backups (default: next to the database)")
	poll := flag.Duration("poll", 60*time.Second, "how often to check GitHub projects for new commits (0 disables)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if restored, err := store.ApplyPendingRestore(*dbPath); err != nil {
		log.Error("apply restored database", "err", err)
		os.Exit(1)
	} else if restored {
		log.Info("restored database from a server backup; the previous one is kept as .pre-restore")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	srv, adminToken, err := server.New(ctx, st, server.Config{
		HeartbeatInterval: *interval,
		AdminToken:        os.Getenv("ORCH_ADMIN_TOKEN"),
		UI:                web.Dist(),
		Version:           version,
		Logger:            log,
		DownloadsDir:      *downloads,
		PollInterval:      pollInterval(*poll),
		BackupDir:         backupDir(*backups, *dbPath),
		BuildDir:          os.Getenv("ORCH_BUILD_DIR"),
		BuildImage:        os.Getenv("ORCH_BUILD_IMAGE"),
		DBPath:            *dbPath,
		Restart:           stop,
		DockerHost:        env("DOCKER_HOST", ""),
		TrustedProxies:    os.Getenv("ORCH_TRUSTED_PROXIES"),
	})
	if err != nil {
		log.Error("start server", "err", err)
		os.Exit(1)
	}
	if adminToken != "" {
		fmt.Fprintf(os.Stderr, "\n  Admin token (shown once, use it to sign in to the dashboard):\n\n    %s\n\n", adminToken)
	}

	go srv.Run(ctx)
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Info("orchestrator listening", "addr", *addr, "version", version)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// pollInterval maps the flag (0 disables) to Config's convention (negative disables).
func pollInterval(d time.Duration) time.Duration {
	if d == 0 {
		return -1
	}
	return d
}

// backupDir keeps backups beside the database unless told otherwise.
func backupDir(flagValue, dbPath string) string {
	if flagValue != "" {
		return flagValue
	}
	return filepath.Join(filepath.Dir(dbPath), "backups")
}

// runSelfUpdate is the updater helper: started by the server from the new image, it swaps
// the server's container and rolls back if the new one doesn't come up healthy.
func runSelfUpdate(args []string) int {
	fs := flag.NewFlagSet("self-update", flag.ExitOnError)
	container := fs.String("container", "", "name of the server container to replace")
	image := fs.String("image", "", "image the new container should run")
	host := fs.String("docker-host", env("DOCKER_HOST", "unix:///var/run/docker.sock"), "Docker engine address")
	var publish multiFlag
	fs.Var(&publish, "publish", "extra TCP port to publish, HOST:CONTAINER (repeatable)")
	var unpublish multiFlag
	fs.Var(&unpublish, "unpublish", "container port whose published mapping is dropped (repeatable)")
	fs.Parse(args)
	d, err := agent.NewDocker(*host)
	if err != nil || *container == "" || *image == "" {
		fmt.Fprintln(os.Stderr, "usage: orchestrator-server self-update -container NAME -image REF")
		return 2
	}
	err = selfupdate.Swap(context.Background(), d, selfupdate.SwapOptions{
		Container: *container, Image: *image, Publish: publish, Unpublish: unpublish,
		Log: func(m string) { fmt.Fprintln(os.Stderr, "self-update:", m) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "self-update failed:", err)
		return 1
	}
	return 0
}

// runDecryptBackup turns a password-protected backup file (.tar.gz.enc) back into a normal
// .tar.gz without a running server, for backups copied off the machine.
func runDecryptBackup(args []string) int {
	fs := flag.NewFlagSet("decrypt-backup", flag.ExitOnError)
	in := fs.String("in", "", "the .tar.gz.enc file")
	out := fs.String("out", "", "where to write the decrypted .tar.gz")
	fs.Parse(args)
	pw := os.Getenv("ORCH_BACKUP_PASSWORD")
	if *in == "" || *out == "" || pw == "" {
		fmt.Fprintln(os.Stderr, "usage: ORCH_BACKUP_PASSWORD=... orchestrator-server decrypt-backup -in FILE.tar.gz.enc -out FILE.tar.gz")
		return 2
	}
	if err := decryptBackup(*in, *out, pw); err != nil {
		fmt.Fprintln(os.Stderr, "decrypt-backup:", err)
		return 1
	}
	return 0
}

func decryptBackup(in, out, password string) error {
	src, err := os.Open(in)
	if err != nil {
		return err
	}
	defer src.Close()
	plain, err := backupcrypt.NewReader(src, password)
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, plain); err != nil {
		dst.Close()
		os.Remove(out)
		return err
	}
	return dst.Close()
}

// runImportBackup stages a server backup or transfer bundle as the next database, the way a
// restore from the dashboard does: the server swaps it in on its next start and keeps the
// database it had as .pre-restore. It is what "install-server.sh --import" and the "promote a
// device" flow use to seed a new server.
func runImportBackup(args []string) int {
	fs := flag.NewFlagSet("import-backup", flag.ExitOnError)
	in := fs.String("in", "", "the backup file (.db, or .db.enc when it is password-protected)")
	db := fs.String("db", env("ORCH_DB", "orchestrator.db"), "the database path of the server that will use it")
	pwFile := fs.String("password-file", "", "file holding the password of a protected backup (or set ORCH_BACKUP_PASSWORD)")
	fs.Parse(args)
	if *in == "" {
		fmt.Fprintln(os.Stderr, "usage: orchestrator-server import-backup -in FILE [-db PATH] [-password-file FILE]")
		return 2
	}
	pw := os.Getenv("ORCH_BACKUP_PASSWORD")
	if *pwFile != "" {
		b, err := os.ReadFile(*pwFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "import-backup:", err)
			return 1
		}
		pw = strings.TrimSpace(string(b))
	}
	if err := importBackup(*in, *db, pw); err != nil {
		fmt.Fprintln(os.Stderr, "import-backup:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "backup staged; it is applied when the server starts")
	return 0
}

func importBackup(in, dbPath, password string) error {
	src, err := os.Open(in)
	if err != nil {
		return err
	}
	defer src.Close()
	var r io.Reader = src
	if strings.HasSuffix(in, ".enc") || password != "" {
		if password == "" {
			return errors.New("this backup is password-protected: give its password")
		}
		if r, err = backupcrypt.NewReader(src, password); err != nil {
			return fmt.Errorf("could not open the backup (wrong password?): %w", err)
		}
	}
	pending := store.PendingRestorePath(dbPath)
	check := pending + ".check"
	defer func() {
		for _, sfx := range []string{"", "-wal", "-shm", ".key", ".key.pre-restore"} {
			os.Remove(check + sfx)
		}
	}()
	out, err := os.OpenFile(check, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := store.CheckSnapshot(check); err != nil {
		return fmt.Errorf("not a usable Orchestrator backup: %w", err)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		os.Remove(check + sfx)
	}
	return os.Rename(check, pending)
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
