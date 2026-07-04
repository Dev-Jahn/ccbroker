// Command ccbrokerd is the credential broker daemon.
//
// Usage:
//
//	ccbrokerd genkey                 # print a new 32-byte master key (hex)
//	ccbrokerd hashtoken              # read a token on stdin, print its sha256
//	ccbrokerd serve -c config.json   # run the daemon
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"ccbroker/internal/config"
	"ccbroker/internal/server"
	"ccbroker/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccbrokerd {genkey|hashtoken|serve} [args]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "genkey":
		genkey()
	case "hashtoken":
		hashtoken()
	case "serve":
		serve(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func genkey() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fatal(err)
	}
	fmt.Println(hex.EncodeToString(b))
}

func hashtoken() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(raw))))
	fmt.Println(hex.EncodeToString(sum[:]))
}

func serve(args []string) {
	cfgPath := ""
	for i := 0; i < len(args); i++ {
		if (args[i] == "-c" || args[i] == "--config") && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		}
	}
	if cfgPath == "" {
		fatal(fmt.Errorf("serve requires -c <config.json>"))
	}
	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		fatal(err)
	}
	key, err := config.LoadKey(cfg.KeyPath)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(cfg.StorePath, key)
	if err != nil {
		fatal(err)
	}
	srv, err := server.New(cfg, st)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go srv.RunRefreshLoop(ctx)
	go srv.RunUsageLoop(ctx)
	if err := srv.Serve(ctx); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ccbrokerd:", err)
	os.Exit(1)
}
