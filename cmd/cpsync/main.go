// Command cpsync shares text and small files between machines on a LAN
// through an encrypted, OTP-protected group-chat web interface.
//
// This file only wires the pieces together; the work happens in the
// internal packages:
//
//	config   defaults, YAML file and flags
//	crypto   key derivation, password hashing, PINs and tokens
//	store    the encrypted SQLite database
//	tlsutil  the HTTPS certificate
//	auth     sessions, lockout and the shared TOTP secret
//	server   the HTTP API and the embedded front end (package web)
//
// Startup sequence:
//
//  1. Read the configuration (defaults < YAML file < flags).
//  2. Open the database: create it on first start (--password), or decrypt
//     it with the key on restart (--key).
//  3. Load or create the TLS certificate.
//  4. Create the server, which loads the shared TOTP secret from the
//     database or, if setup has never been done, generates a setup PIN.
//  5. Print the address (and the PIN) and serve until Ctrl+C / SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/pycloux/cpsync/internal/config"
	"github.com/pycloux/cpsync/internal/crypto"
	"github.com/pycloux/cpsync/internal/server"
	"github.com/pycloux/cpsync/internal/store"
	"github.com/pycloux/cpsync/internal/tlsutil"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run performs the startup sequence described in the package comment and
// blocks until the server stops. Errors are returned to main, which prints
// them and exits with status 1.
func run() error {
	// 1. Configuration.
	cfg, opts, err := config.Parse(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if opts.ShowVersion {
		fmt.Println("cpsync", version)
		return nil
	}
	autoBind := cfg.Bind == ""
	if autoBind {
		cfg.Bind = config.DetectLANIP()
	}

	// ctx is cancelled by Ctrl+C or SIGTERM. It is set up before anything is
	// written, so that an interrupted first start can clean up after itself
	// (see firstStart), and it later triggers a graceful shutdown in
	// srv.Serve.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 2. Database. Everything persistent lives in this one encrypted file.
	st, err := openStore(ctx, cfg, opts)
	if err != nil {
		return err
	}
	defer st.Close()

	// 3. TLS certificate (custom, or self-signed and kept in the database).
	tlsRes, err := tlsutil.Load(ctx, cfg.TLSCert, cfg.TLSKey, tlsutil.Hosts(cfg.Bind), st)
	if err != nil {
		return err
	}

	// 4. Server. All logging goes to the console; there is no log file.
	logger := log.New(os.Stdout, "", log.LstdFlags)
	srv, err := server.New(cfg, st, logger, version)
	if err != nil {
		return err
	}

	// 5. Tell the operator how to reach the server, then serve.
	fmt.Printf("\ncpsync %s\n%s", version, cfg.Describe())
	switch {
	case tlsRes.Generated:
		fmt.Println("\nGenerated a new self-signed TLS certificate.")
	case tlsRes.SelfSigned:
		fmt.Println("\nUsing the stored self-signed TLS certificate.")
	}
	fmt.Printf("Certificate SHA-256 fingerprint:\n  %s\n", tlsRes.Fingerprint)
	if w := loopbackWarning(cfg.Bind, autoBind); w != "" {
		fmt.Fprint(os.Stderr, w)
		fmt.Printf("\nOpen https://%s/ in a browser on this machine. Press Ctrl+C to stop.\n", srv.PublicHost())
	} else {
		fmt.Printf("\nOpen https://%s/ in a browser on your LAN. Press Ctrl+C to stop.\n", srv.PublicHost())
	}
	if pin := srv.SetupPIN(); pin != "" {
		fmt.Println()
		fmt.Println("--------------------------------------------------------------------------------")
		fmt.Printf(" SETUP PIN:  %s\n", pin)
		fmt.Println(" The first person to open the page enters this PIN to show the QR code")
		fmt.Println(" of the admin account, then signs in as \"admin\" and adds the other users.")
		fmt.Println("--------------------------------------------------------------------------------")
	}
	fmt.Println()

	// Open the port before saying so: scripts and tests wait for the
	// "listening on" line and connect straight away.
	ln, err := net.Listen("tcp", srv.Addr())
	if err != nil {
		return err
	}
	logger.Printf("listening on %s", srv.Addr())
	if err := srv.ServeListener(ctx, ln, tlsRes.Certificate); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Printf("server stopped")
	return nil
}

// openStore creates the database on first start (--password) or opens it on
// restart (--key). The values can also come from the CPSYNC_PASSWORD and
// CPSYNC_KEY environment variables, which keeps them out of shell history.
func openStore(ctx context.Context, cfg config.Config, opts config.Options) (*store.Store, error) {
	password, key := opts.Password, opts.Key
	if password == "" && key == "" {
		password, key = os.Getenv("CPSYNC_PASSWORD"), os.Getenv("CPSYNC_KEY")
	}
	switch {
	case password != "" && key != "":
		return nil, errors.New("use either --password (first start) or --key (restart), not both")
	case password != "":
		return firstStart(ctx, cfg, password)
	case key != "":
		k, err := crypto.DecodeKey(key)
		if err != nil {
			return nil, err
		}
		st, err := store.Open(cfg.Database, k)
		if err != nil {
			return nil, err
		}
		fmt.Printf("Database %s unlocked.\n", cfg.Database)
		return st, nil
	default:
		return nil, errors.New("missing --password (first start) or --key (restart); run cpsync --help")
	}
}

// errInterrupted is returned when Ctrl+C or SIGTERM arrives during a first
// start, before the key has been printed.
var errInterrupted = errors.New("first start interrupted: no database was created; start again with --password")

// firstStart creates a new database. The key is derived from the password
// plus random data, so it cannot be recomputed from the password later:
// it is printed once, and the operator must keep it to restart the server.
// Only a hash of the password is stored, for the admin unlock in the UI.
//
// First start is all or nothing: a database is left on disk only if it is
// complete (admin hash stored) and its key has been printed. A database
// without its key, or without the admin hash, could neither be used nor
// recreated (Create refuses an existing file), so any failure or
// interruption (ctx cancelled by Ctrl+C) before the key is printed removes
// it. The slow steps (Argon2) cannot be interrupted mid-way, so ctx is
// checked between steps.
func firstStart(ctx context.Context, cfg config.Config, password string) (*store.Store, error) {
	key, err := crypto.DeriveKey(password)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, errInterrupted
	}
	st, err := store.Create(cfg.Database, key)
	if err != nil {
		return nil, err
	}
	abandon := func(err error) (*store.Store, error) {
		st.Close()
		if rmErr := store.Remove(cfg.Database); rmErr != nil {
			return nil, fmt.Errorf("%w (and the incomplete database could not be removed: %v; delete %s by hand)", err, rmErr, cfg.Database)
		}
		return nil, err
	}

	hash, err := crypto.HashPassword(password)
	if err == nil {
		err = st.SetSetting(context.Background(), store.SettingAdminHash, []byte(hash))
	}
	if err != nil {
		return abandon(fmt.Errorf("store admin password: %w", err))
	}
	// Last chance to give up cleanly: once the key is printed, the database
	// is complete and usable, so it is kept.
	if ctx.Err() != nil {
		return abandon(errInterrupted)
	}

	enc := crypto.EncodeKey(key)
	fmt.Println("================================================================================")
	fmt.Printf(" Database %s created.\n\n", cfg.Database)
	fmt.Println(" DATABASE KEY - keep it safe, it is needed to restart the server.")
	fmt.Println(" If it is lost, the database cannot be recovered.")
	fmt.Println()
	fmt.Printf("   %s\n\n", enc)
	fmt.Printf(" Restart with:  cpsync --key %s\n", enc)
	fmt.Println("================================================================================")
	return st, nil
}

// loopbackWarning returns a warning when the server would only be reachable
// from this machine because no LAN address was found (autoBind: the address
// was detected, not chosen with --bind). It returns "" otherwise.
func loopbackWarning(bind string, autoBind bool) string {
	ip := net.ParseIP(bind)
	if !autoBind || ip == nil || !ip.IsLoopback() {
		return ""
	}
	return "\nWARNING: no network address was found, so cpsync listens on " + bind + " only.\n" +
		"         Other machines cannot connect. Check the network connection, or choose\n" +
		"         an address with --bind (for example --bind 0.0.0.0 for every interface).\n"
}
