package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"fast-sandbox/internal/sandboxinit"
)

func main() {
	var configPath string
	var userUID uint
	var userGID uint
	var additionalGIDs string
	flag.StringVar(&configPath, "config", "/.fast/run/infra.json", "Runtime Augmentation instance config.")
	flag.UintVar(&userUID, "user-uid", 0, "Original OCI user UID.")
	flag.UintVar(&userGID, "user-gid", 0, "Original OCI user GID.")
	flag.StringVar(&additionalGIDs, "user-additional-gids", "", "Comma-separated original OCI supplementary groups.")
	flag.Parse()
	config, err := sandboxinit.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-init: load config: %v\n", err)
		os.Exit(1)
	}
	groups, err := parseAdditionalGIDs(additionalGIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-init: parse user groups: %v\n", err)
		os.Exit(1)
	}
	config.UserCredential = &sandboxinit.UserCredential{UID: uint32(userUID), GID: uint32(userGID), AdditionalGIDs: groups}
	userArgs := flag.Args()
	if len(userArgs) > 0 && userArgs[0] == "--" {
		userArgs = userArgs[1:]
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor := sandboxinit.NewSupervisor(os.Stdout, os.Stderr)
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	go func() {
		for received := range signals {
			supervisor.Forward(received)
			switch received {
			case syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT:
				cancel()
			}
		}
	}()
	exitCode, runErr := supervisor.Run(ctx, config, userArgs)
	if runErr != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "sandbox-init: %v\n", runErr)
	}
	os.Exit(exitCode)
}

func parseAdditionalGIDs(value string) ([]uint32, error) {
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	groups := make([]uint32, 0, len(parts))
	for _, part := range parts {
		parsed, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid group %q: %w", part, err)
		}
		groups = append(groups, uint32(parsed))
	}
	return groups, nil
}
