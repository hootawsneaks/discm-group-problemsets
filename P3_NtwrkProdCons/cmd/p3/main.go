// Command p3 runs either side of the media upload system:
//
//	p3 consumer   gRPC upload server + web UI
//	p3 producer   reads videos from <input>/p<i> and uploads them
package main

import (
	"errors"
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

// Upper bounds on the thread counts and the queue. The lower bound catches a
// missing setting; these catch a typo. max_queue is allocated up front (the
// queue is a buffered channel), so an extra zero would reserve gigabytes
// before the first upload arrives, and each consumer thread can run an ffmpeg
// process of its own.
const (
	maxThreads     = 256
	maxQueueLength = 100_000
	maxCRF         = 51 // x264 accepts 0 (lossless) to 51 (worst)
)

// check rejects settings that would make the program misbehave later, so a bad
// config file fails at startup with one clear message instead of failing per
// upload.
func check(cfg config.Config) error {
	switch {
	case cfg.Producers < 1, cfg.Consumers < 1, cfg.QueueSize < 1:
		return errors.New("producers, consumers and max_queue must all be >= 1")
	case cfg.Producers > maxThreads:
		return fmt.Errorf("producers is %d, the most is %d", cfg.Producers, maxThreads)
	case cfg.Consumers > maxThreads:
		return fmt.Errorf("consumers is %d, the most is %d", cfg.Consumers, maxThreads)
	case cfg.QueueSize > maxQueueLength:
		return fmt.Errorf("max_queue is %d, the most is %d", cfg.QueueSize, maxQueueLength)
	case cfg.CompressCRF < 0 || cfg.CompressCRF > maxCRF:
		return fmt.Errorf("compress_crf is %d, it must be 0-%d", cfg.CompressCRF, maxCRF)
	case cfg.MaxUploadBytes < 1:
		return errors.New("max_upload_bytes must be >= 1")
	}
	return nil
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
	if err := check(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", *cfgPath, err)
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
