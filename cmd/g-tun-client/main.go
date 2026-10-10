// Command g-tun-client runs the G-Tun client.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mahdi-1991/G-tun/internal/client"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/logging"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "g-tun-client:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "client_config.json", "path to the client configuration file")
	debug := flag.Bool("debug", false, "enable debug logging")
	checkOnly := flag.Bool("check", false, "validate the configuration and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	logging.Init(*debug)

	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Println("configuration is valid")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return client.Run(ctx, cfg)
}
