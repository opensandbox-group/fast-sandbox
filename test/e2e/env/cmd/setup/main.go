package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	e2eenv "fast-sandbox/test/e2e/env"
)

func main() {
	profile := flag.String("profile", string(e2eenv.ProfileBasic), "kind runtime profile: basic, gvisor, kata-qemu, kata-clh, kata-fc")
	timeout := flag.Duration("timeout", 20*time.Minute, "maximum time to prepare the e2e environment")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	manager, err := e2eenv.NewManager(
		e2eenv.Profile(*profile),
		e2eenv.WithProgressWriter(os.Stdout),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create e2e environment manager: %v\n", err)
		os.Exit(1)
	}
	if err := manager.Ensure(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ensure e2e environment: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("kind environment ready: profile=%s\n", *profile)
}
