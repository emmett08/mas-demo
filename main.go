package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rollout demo:", err)
		os.Exit(1)
	}
}

func run() error {
	binary := flag.String("mas-bin", "mas", "path to the native MAS administrative CLI")
	name := flag.String("scenario", "healthy", "healthy, capacity, unsafe-image, or missing-telemetry")
	directory := flag.String("state-dir", "", "new private directory for retained evidence (default: temporary directory)")
	timeout := flag.Duration("timeout", 30*time.Second, "total workflow time limit (maximum 2m)")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *timeout <= 0 || *timeout > 2*time.Minute {
		return fmt.Errorf("timeout must be positive and no greater than 2m")
	}
	regions, err := scenario(*name)
	if err != nil {
		return err
	}
	resolved, err := exec.LookPath(*binary)
	if err != nil {
		return fmt.Errorf("MAS CLI not found; build emmett08/mas and supply -mas-bin: %w", err)
	}
	var dir string
	if *directory == "" {
		dir, err = os.MkdirTemp("", "mas-rollout-")
	} else {
		dir, err = filepath.Abs(*directory)
		if err == nil {
			err = os.Mkdir(dir, 0700)
		}
	}
	if err != nil {
		return fmt.Errorf("create fresh state directory: %w", err)
	}
	fmt.Fprintln(os.Stderr, "Retained MAS evidence:", dir)
	config, err := json.MarshalIndent(rolloutTopology(), "", "  ")
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "topology.json")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		return err
	}
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	b := &bus{ctx: ctx, binary: resolved, database: filepath.Join(dir, "state.sqlite")}
	if err := b.command(nil, nil, "init", b.database, "rollout", configPath); err != nil {
		return err
	}
	result, err := runRollout(b, *name, regions)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
