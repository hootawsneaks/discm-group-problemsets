// Command p3 runs either side of the media upload system:
//
//	p3 consumer   gRPC upload server + web UI
//	p3 producer   reads videos from <input>/p<i> and uploads them
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"p3/internal/config"
	"p3/internal/consumer"
	"p3/internal/producer"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  %[1]s consumer [-config config.json]
  %[1]s producer [-config config.json]

All settings come from the config file.
`, os.Args[0])
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	mode := os.Args[1]

	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "path to config file")
	fs.Parse(os.Args[2:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not load %s: %v\n", *cfgPath, err)
		os.Exit(1)
	}
	if cfg.ConnectWait.Duration <= 0 {
		cfg.ConnectWait = config.Duration{Duration: 60 * time.Second}
	}
	if cfg.Producers < 1 || cfg.Consumers < 1 || cfg.QueueSize < 1 {
		fmt.Fprintln(os.Stderr, "producers, consumers and max_queue must all be >= 1")
		os.Exit(1)
	}

	switch mode {
	case "consumer":
		err = consumer.Run(cfg)
	case "producer":
		err = producer.Run(cfg)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
