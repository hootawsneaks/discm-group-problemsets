// Package config loads config.json. Node addresses can be overridden with
// environment variables (P4_AUTH_ADDR, P4_COURSE_ADDR, P4_GRADE_ADDR) so the
// same file works locally and in docker-compose.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// utf8BOM is what Windows editors put at the start of a UTF-8 file.
// encoding/json chokes on it, so it is trimmed.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

type Config struct {
	ViewListen string `json:"view_listen"` // HTTPS listen address of the view node
	AuthAddr   string `json:"auth_addr"`   // host:port of the auth node
	CourseAddr string `json:"course_addr"` // host:port of the course node
	GradeAddr  string `json:"grade_addr"`  // host:port of the grade node
	JWTSecret  string `json:"jwt_secret"`  // shared by every node to verify tokens
	TokenTTL   string `json:"token_ttl"`   // e.g. "1h"
	RPCTimeout string `json:"rpc_timeout"` // per gRPC call from the view, e.g. "2s"
	DataDir    string `json:"data_dir"`    // each node keeps its own <role>.json here

	// HTTPS for the view node. The certificate is self-signed and generated on
	// first start, so the browser shows HTTPS (with a one-time warning).
	TLSCert  string   `json:"tls_cert"`
	TLSKey   string   `json:"tls_key"`
	TLSHosts []string `json:"tls_hosts"` // extra names/IPs for the certificate (e.g. the VM's IP)

	// PoolSize is how many gRPC connections the view keeps open per node
	// (bonus 3: connection pooling).
	PoolSize int `json:"pool_size"`

	// RevocationCache is how long the course and grade nodes reuse their last
	// "this session is live" answer before asking the auth node again. Zero
	// (the default) means ask every time, so a logout takes effect everywhere
	// at once; anything higher is a window in which it is not noticed yet.
	RevocationCache string `json:"revocation_cache"`

	// RevocationTimeout is how long those nodes wait for that answer. It must
	// stay well under rpc_timeout: a node that is switched off swallows
	// connections rather than refusing them, so a long wait here would eat the
	// caller's whole budget and make this node look down too.
	RevocationTimeout string `json:"revocation_timeout"`

	TTL                  time.Duration `json:"-"`
	Timeout              time.Duration `json:"-"`
	RevocationCacheTTL   time.Duration `json:"-"`
	RevocationTimeoutDur time.Duration `json:"-"`
}

func Load(path string) (*Config, error) {
	c := &Config{
		ViewListen:        ":8080",
		AuthAddr:          "localhost:50051",
		CourseAddr:        "localhost:50052",
		GradeAddr:         "localhost:50053",
		TokenTTL:          "1h",
		RPCTimeout:        "2s",
		DataDir:           "data",
		TLSCert:           "certs/server.crt",
		TLSKey:            "certs/server.key",
		PoolSize:          4,
		RevocationCache:   "0s",
		RevocationTimeout: "500ms",
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, utf8BOM), c); err != nil {
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
	if c.RevocationCacheTTL, err = time.ParseDuration(c.RevocationCache); err != nil {
		return nil, fmt.Errorf("revocation_cache: %w", err)
	}
	if c.RevocationTimeoutDur, err = time.ParseDuration(c.RevocationTimeout); err != nil {
		return nil, fmt.Errorf("revocation_timeout: %w", err)
	}
	if c.RevocationTimeoutDur >= c.Timeout {
		return nil, fmt.Errorf(
			"revocation_timeout (%s) must be shorter than rpc_timeout (%s), or a node that cannot "+
				"reach the auth node will look down itself", c.RevocationTimeout, c.RPCTimeout)
	}
	if c.PoolSize < 1 {
		c.PoolSize = 1
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
