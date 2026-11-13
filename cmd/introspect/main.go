// introspect serves a private reader for a directory of org-mode files and
// charts journal entries graded by a Kev model.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ryansghost/journal-introspection/internal/config"
	"github.com/ryansghost/journal-introspection/internal/grader"
	"github.com/ryansghost/journal-introspection/internal/indexer"
	"github.com/ryansghost/journal-introspection/internal/store"
	"github.com/ryansghost/journal-introspection/internal/web"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "hash-password":
		err = hashPassword()
	case "healthcheck":
		err = healthcheck()
	default:
		err = fmt.Errorf("unknown command %q; use serve, hash-password or healthcheck", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "introspect:", err)
		os.Exit(1)
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rubric, err := grader.LoadRubric(cfg.RubricPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g := grader.New(db, rubric, cfg.KevURL, cfg.KevModel)
	ix := indexer.New(db, cfg.OrgRoot, cfg.Exclude)
	ix.OnChange = g.Wake
	srv, err := web.New(cfg, db, ix, g)
	if err != nil {
		return err
	}
	go ix.Run(ctx, cfg.ScanInterval)
	go g.Run(ctx)

	if cfg.AuthDisabled {
		slog.Warn("AUTH_DISABLED=1: anyone who can reach this port can read everything")
	} else if cfg.TOTPSecret == "" {
		slog.Warn("AUTH_TOTP_SECRET is not set: login uses the password alone")
	}
	hs := &http.Server{Addr: cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shut)
	}()
	slog.Info("listening", "addr", cfg.Addr, "org", cfg.OrgRoot, "rubric", rubric.Version, "kev", cfg.KevURL)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// hashPassword reads a password from standard input and prints the two
// settings the login needs.
func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		if err != nil {
			return err
		}
		return errors.New("empty password")
	}
	if len(password) < 12 {
		fmt.Fprintln(os.Stderr, "Warning: that is short for something facing the internet. Use a long passphrase.")
	}
	secret := web.NewTOTPSecret()
	fmt.Printf("AUTH_PASSWORD_HASH=%s\n", web.HashPassword(password))
	fmt.Printf("AUTH_TOTP_SECRET=%s\n", secret)
	fmt.Fprintf(os.Stderr, "\nAdd the secret to an authenticator app by hand, or as this link:\notpauth://totp/Introspection?secret=%s&issuer=%s\n", secret, url.QueryEscape("Introspection"))
	return nil
}

// healthcheck lets Docker probe the server from inside a scratch image.
func healthcheck() error {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
