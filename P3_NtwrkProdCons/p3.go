package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// Config is shared by both the producer and consumer programs.
//   - Producers (p): number of producer threads; producer i reads videos from <input_dir>/p<i>
//   - Consumers (c): number of consumer threads that drain the queue
//   - QueueSize (q): max queue length (leaky bucket capacity); uploads beyond it are dropped
type Config struct {
	Producers  int    `json:"producers"`
	Consumers  int    `json:"consumers"`
	QueueSize  int    `json:"max_queue"`
	Address    string `json:"address"`     // gRPC address: consumer listens on it, producer dials it
	HTTPAddr   string `json:"http_addr"`   // consumer web UI address
	InputDir   string `json:"input_dir"`   // producer: root folder holding p1, p2, ... subfolders
	UploadDir  string `json:"upload_dir"`  // consumer: single folder where uploaded videos are saved
	PreviewDir string `json:"preview_dir"` // consumer: generated 10-second previews
}

func defaultConfig() Config {
	return Config{
		Producers:  1,
		Consumers:  1,
		QueueSize:  5,
		Address:    "localhost:50051",
		HTTPAddr:   ":8080",
		InputDir:   "videos",
		UploadDir:  "uploads",
		PreviewDir: "previews",
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  %[1]s consumer [-config config.json] [-c N] [-q N] [-addr host:port] [-http :8080]
  %[1]s producer [-config config.json] [-p N] [-addr host:port] [-input videos]
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
	addr := fs.String("addr", "", "gRPC address (overrides config)")
	httpAddr := fs.String("http", "", "web UI address (overrides config)")
	input := fs.String("input", "", "producer input root folder (overrides config)")
	fs.Parse(os.Args[2:])

	cfg, err := loadConfig(*cfgPath)
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
	if *addr != "" {
		cfg.Address = *addr
	}
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}
	if *input != "" {
		cfg.InputDir = *input
	}
	if cfg.Producers < 1 || cfg.Consumers < 1 || cfg.QueueSize < 1 {
		fmt.Fprintln(os.Stderr, "producers, consumers and max_queue must all be >= 1")
		os.Exit(1)
	}

	switch mode {
	case "consumer":
		err = runConsumer(cfg)
	case "producer":
		err = runProducer(cfg)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
