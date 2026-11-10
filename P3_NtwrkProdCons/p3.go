package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Producers int `json:"producers"`
	Consumers int `json:"consumers"`
	QueueSize int `json:"max_queue"`
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

func main() {
	path, err := os.Getwd()
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("Current working directory:", path)
	path += "/config.json"
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(cfg.Consumers)
	fmt.Println(cfg.Producers)
	fmt.Println(cfg.QueueSize)
}
