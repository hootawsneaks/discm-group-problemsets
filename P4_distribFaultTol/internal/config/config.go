// Package config loads config.json. Node addresses can be overridden with
// environment variables (P4_AUTH_ADDR, P4_COURSE_ADDR, P4_GRADE_ADDR) so the
// same file works locally and in docker-compose.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

type Config struct {
	ViewListen string `json:"view_listen"` // HTTP listen address of the view node
	AuthAddr   string `json:"auth_addr"`   // host:port of the auth node
	CourseAddr string `json:"course_addr"` // host:port of the course node
	GradeAddr  string `json:"grade_addr"`  // host:port of the grade node
	JWTSecret  string `json:"jwt_secret"`  // shared by every node to verify tokens
	TokenTTL   string `json:"token_ttl"`   // e.g. "1h"
	RPCTimeout string `json:"rpc_timeout"` // per gRPC call from the view, e.g. "2s"
	DataDir    string `json:"data_dir"`    // each node keeps its own <role>.json here

	TTL     time.Duration `json:"-"`
	Timeout time.Duration `json:"-"`
}

func Load(path string) (*Config, error) {
	c := &Config{
		ViewListen: ":8080",
		AuthAddr:   "localhost:50051",
		CourseAddr: "localhost:50052",
		GradeAddr:  "localhost:50053",
		TokenTTL:   "1h",
		RPCTimeout: "2s",
		DataDir:    "data",
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for env, dst := range map[string]*string{
		"P4_AUTH_ADDR":   &c.AuthAddr,
		"P4_COURSE_ADDR": &c.CourseAddr,
		"P4_GRADE_ADDR":  &c.GradeAddr,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if c.JWTSecret == "" {
		return nil, fmt.Errorf("%s: jwt_secret must be set", path)
	}
	if c.TTL, err = time.ParseDuration(c.TokenTTL); err != nil {
		return nil, fmt.Errorf("token_ttl: %w", err)
	}
	if c.Timeout, err = time.ParseDuration(c.RPCTimeout); err != nil {
		return nil, fmt.Errorf("rpc_timeout: %w", err)
	}
	return c, nil
}

// ListenAddr turns "host:port" into ":port" so a node listens on all interfaces.
func ListenAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return ":" + port
}
