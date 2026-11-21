// Package config loads config.json. Node addresses can be overridden with
// environment variables (P4_AUTH_ADDR, P4_COURSE_ADDR, P4_GRADE_ADDR,
// P4_ANNOUNCE_ADDR, and P4_DB_HOSTS as a comma-separated list) so the same file
// works locally and in docker-compose.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
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
	// AnnounceAddr is the host:port of the announce node (registrar
	// announcements), the extra feature on its own node.
	AnnounceAddr string `json:"announce_addr"`
	JWTSecret    string `json:"jwt_secret"`  // shared by every node to verify tokens
	TokenTTL     string `json:"token_ttl"`   // e.g. "1h"
	RPCTimeout   string `json:"rpc_timeout"` // per gRPC call from the view, e.g. "2s"

	// The replicated database: a PostgreSQL primary and its replica. Every
	// server is listed; a node only uses the one that accepts writes, so
	// after repmgr promotes the replica, the nodes follow by themselves.
	DBHosts    []string `json:"db_hosts"`    // host:port of every database server
	DBPassword string   `json:"db_password"` // every node's database user has this password

	// Database connection pooling: each node keeps one pool of connections
	// to its own database.
	DBMaxOpenConns    int    `json:"db_max_open_conns"`     // at most this many connections at once
	DBMaxIdleConns    int    `json:"db_max_idle_conns"`     // kept open while idle, ready for the next request
	DBConnMaxLifetime string `json:"db_conn_max_lifetime"`  // a connection is replaced after this long
	DBConnMaxIdleTime string `json:"db_conn_max_idle_time"` // an unused connection is closed after this long
	DBConnectTimeout  string `json:"db_connect_timeout"`    // per server, when opening a new connection
	DBProbeTimeout    string `json:"db_probe_timeout"`      // status page: how long to wait for each server

	// HTTPS for the view node. The certificate is self-signed and generated on
	// first start, so the browser shows HTTPS (with a one-time warning).
	TLSCert  string   `json:"tls_cert"`
	TLSKey   string   `json:"tls_key"`
	TLSHosts []string `json:"tls_hosts"` // extra names/IPs for the certificate (e.g. the VM's IP)

	// PoolSize is how many gRPC connections the view keeps open per node.
	// (This pools node-to-node gRPC connections; it is not the database
	// connection pool.)
	PoolSize int `json:"pool_size"`

	// Timezone is used to read and show announcement dates, e.g. "Asia/Manila".
	Timezone string `json:"timezone"`

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

	TTL                  time.Duration  `json:"-"`
	Timeout              time.Duration  `json:"-"`
	RevocationCacheTTL   time.Duration  `json:"-"`
	RevocationTimeoutDur time.Duration  `json:"-"`
	Location             *time.Location `json:"-"`
	DBConnMaxLifetimeDur time.Duration  `json:"-"`
	DBConnMaxIdleTimeDur time.Duration  `json:"-"`
	DBConnectTimeoutDur  time.Duration  `json:"-"`
	DBProbeTimeoutDur    time.Duration  `json:"-"`
}

func Load(path string) (*Config, error) {
	c := &Config{
		ViewListen:        ":8080",
		AuthAddr:          "localhost:50051",
		CourseAddr:        "localhost:50052",
		GradeAddr:         "localhost:50053",
		AnnounceAddr:      "localhost:50054",
		Timezone:          "Asia/Manila",
		TokenTTL:          "1h",
		RPCTimeout:        "2s",
		DBHosts:           []string{"localhost:5441", "localhost:5442"},
		DBMaxOpenConns:    10,
		DBMaxIdleConns:    5,
		DBConnMaxLifetime: "30m",
		DBConnMaxIdleTime: "5m",
		DBConnectTimeout:  "500ms",
		DBProbeTimeout:    "500ms",
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
		"P4_AUTH_ADDR":     &c.AuthAddr,
		"P4_COURSE_ADDR":   &c.CourseAddr,
		"P4_GRADE_ADDR":    &c.GradeAddr,
		"P4_ANNOUNCE_ADDR": &c.AnnounceAddr,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("P4_DB_HOSTS"); v != "" {
		c.DBHosts = strings.Split(v, ",")
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
	for name, d := range map[string]struct {
		in  string
		out *time.Duration
	}{
		"db_conn_max_lifetime":  {c.DBConnMaxLifetime, &c.DBConnMaxLifetimeDur},
		"db_conn_max_idle_time": {c.DBConnMaxIdleTime, &c.DBConnMaxIdleTimeDur},
		"db_connect_timeout":    {c.DBConnectTimeout, &c.DBConnectTimeoutDur},
		"db_probe_timeout":      {c.DBProbeTimeout, &c.DBProbeTimeoutDur},
	} {
		if *d.out, err = time.ParseDuration(d.in); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if len(c.DBHosts) == 0 {
		return nil, fmt.Errorf("%s: db_hosts must list the database servers", path)
	}
	// A server that is switched off swallows connection attempts, so these
	// waits must leave room in the caller's budget, as with revocation. A new
	// connection may try every server in turn, so it is the total that counts:
	// otherwise, with the database down, the node itself would look down.
	if total := c.DBConnectTimeoutDur * time.Duration(len(c.DBHosts)); total >= c.Timeout || c.DBProbeTimeoutDur >= c.Timeout {
		return nil, fmt.Errorf("db_connect_timeout (%s) x %d servers and db_probe_timeout (%s) must each be shorter than rpc_timeout (%s)",
			c.DBConnectTimeout, len(c.DBHosts), c.DBProbeTimeout, c.RPCTimeout)
	}
	if c.DBMaxOpenConns < 1 {
		c.DBMaxOpenConns = 1
	}
	if c.DBMaxIdleConns > c.DBMaxOpenConns {
		c.DBMaxIdleConns = c.DBMaxOpenConns
	}
	if c.Location, err = time.LoadLocation(c.Timezone); err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
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
