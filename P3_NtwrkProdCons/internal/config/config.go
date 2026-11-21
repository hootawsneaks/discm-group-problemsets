// Package config holds the settings shared by the producer and consumer.
package config

import (
	"encoding/json"
	"os"
	"time"
)

// Config is shared by both the producer and consumer programs.
//   - Producers (p): number of producer threads; producer i reads videos from <input_dir>/p<i>
//   - Consumers (c): number of consumer threads that drain the queue
//   - QueueSize (q): max queue length (leaky bucket capacity); uploads beyond it are dropped
type Config struct {
	Producers int `json:"producers"`
	Consumers int `json:"consumers"`
	QueueSize int `json:"max_queue"`

	// Consumer side
	ListenAddr string `json:"listen_addr"` // gRPC address the consumer listens on (":50051" = all interfaces)
	HTTPAddr   string `json:"http_addr"`   // web UI address
	UploadDir  string `json:"upload_dir"`  // single folder where uploaded videos are saved
	PreviewDir string `json:"preview_dir"` // generated 10-second previews
	ConvertDir string `json:"convert_dir"` // browser-playable copies of non-mp4/webm uploads

	// Producer side
	ConsumerAddr string   `json:"consumer_addr"` // gRPC address of the consumer the producer dials
	InputDir     string   `json:"input_dir"`     // root folder holding p1, p2, ... subfolders
	ConnectWait  Duration `json:"connect_wait"`  // how long to wait for the consumer to come up
}

// Duration lets durations be written as "60s" in config.json.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

func Default() Config {
	return Config{
		Producers:    1,
		Consumers:    1,
		QueueSize:    5,
		ListenAddr:   ":50051",
		HTTPAddr:     ":8080",
		UploadDir:    "data/uploads",
		PreviewDir:   "data/previews",
		ConvertDir:   "data/converted",
		ConsumerAddr: "localhost:50051",
		InputDir:     "videos",
		ConnectWait:  Duration{60 * time.Second},
	}
}

// Load reads path over the defaults. On error the defaults are returned.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	// CONSUMER_ADDR overrides consumer_addr, so docker-compose.yml can declare the IP.
	if addr := os.Getenv("CONSUMER_ADDR"); addr != "" {
		cfg.ConsumerAddr = addr
	}
	return cfg, err
}
