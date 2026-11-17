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
  %[1]s consumer [-config config.json] [-c N] [-q N] [-listen :50051] [-http :8080]
  %[1]s producer [-config config.json] [-p N] [-addr host:port] [-input videos] [-wait 60s]
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
	p := fs.Int("p", 0, "number of producer threads (overrides config)")
	c := fs.Int("c", 0, "number of consumer threads (overrides config)")
	q := fs.Int("q", 0, "max queue length (overrides config)")
	listen := fs.String("listen", "", "consumer: gRPC listen address (overrides config)")
	httpAddr := fs.String("http", "", "consumer: web UI address (overrides config)")
	addr := fs.String("addr", "", "producer: consumer's gRPC address host:port (overrides config)")
	input := fs.String("input", "", "producer: input root folder (overrides config)")
	wait := fs.Duration("wait", 0, "producer: how long to wait for the consumer (overrides config)")
	fs.Parse(os.Args[2:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load %s (%v), using defaults\n", *cfgPath, err)
	}
	if *p > 0 {
		cfg.Producers = *p
	}
	if *c > 0 {
		cfg.Consumers = *c
	}
	if *q > 0 {
		cfg.QueueSize = *q
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}
	if *addr != "" {
		cfg.ConsumerAddr = *addr
	}
	if *input != "" {
		cfg.InputDir = *input
	}
	if *wait > 0 {
		cfg.ConnectWait = config.Duration{Duration: *wait}
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
