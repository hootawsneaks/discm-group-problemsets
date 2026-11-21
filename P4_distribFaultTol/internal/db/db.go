// Package db is how a node reaches its database: a PostgreSQL primary with a
// streaming replica, where repmgr promotes the replica if the primary dies.
//
// Two things live here:
//
//   - Database connection pooling. Each node keeps one *sql.DB, which is a
//     pool: up to db_max_open_conns connections, db_max_idle_conns of them
//     kept open while idle, each one replaced after db_conn_max_lifetime and
//     closed after db_conn_max_idle_time unused. Requests borrow a
//     connection and hand it back instead of connecting every time.
//
//   - Client-side failover. Every database server is listed, and a new
//     connection is only accepted from the one that takes writes
//     (target_session_attrs=read-write), so after a failover new connections
//     go to the promoted replica by themselves. The server that last worked
//     is tried first, so a dead primary costs a wait only once, not on every
//     new connection.
//
// Each node has its own database and its own database user, so no node can
// read another node's data even though they share the replicated servers.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/config"
	pb "p4/proto"
)

// DB is one node's connection pool to its own database.
type DB struct {
	*sql.DB
	name    string
	hosts   []string
	cfg     *config.Config
	connCfg *pgx.ConnConfig // for probing each server on the status page
	setup   func(context.Context, *sql.Tx) error

	lastGood atomic.Value // string: host:port that last accepted a connection
	ready    atomic.Bool
	setupMu  sync.Mutex
}

// Open creates the pool for the node called name; the database and user are
// both called name. Nothing is dialled yet, so a node starts even while every
// database server is down. setup creates the node's tables and seeds them; it
// runs once, in a transaction, the first time the database is reachable.
func Open(cfg *config.Config, name string, setup func(context.Context, *sql.Tx) error) (*DB, error) {
	connCfg, err := pgx.ParseConfig(dsn(cfg, name))
	if err != nil {
		return nil, fmt.Errorf("database config: %w", err)
	}
	d := &DB{name: name, hosts: cfg.DBHosts, cfg: cfg, connCfg: connCfg, setup: setup}
	d.DB = stdlib.OpenDB(*connCfg,
		stdlib.OptionBeforeConnect(d.lastGoodFirst),
		stdlib.OptionAfterConnect(d.remember))

	// Database connection pooling.
	d.SetMaxOpenConns(cfg.DBMaxOpenConns)
	d.SetMaxIdleConns(cfg.DBMaxIdleConns)
	d.SetConnMaxLifetime(cfg.DBConnMaxLifetimeDur)
	d.SetConnMaxIdleTime(cfg.DBConnMaxIdleTimeDur)
	return d, nil
}

func dsn(cfg *config.Config, name string) string {
	var hosts, ports []string
	for _, hp := range cfg.DBHosts {
		h, p, err := net.SplitHostPort(hp)
		if err != nil {
			h, p = hp, "5432"
		}
		hosts, ports = append(hosts, h), append(ports, p)
	}
	secs := max(1, int(cfg.DBConnectTimeoutDur.Round(time.Second)/time.Second))
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable "+
		"target_session_attrs=read-write connect_timeout=%d application_name=p4-%s",
		strings.Join(hosts, ","), strings.Join(ports, ","), name, cfg.DBPassword, name, secs, name)
}

// lastGoodFirst reorders the servers so the one that last worked is tried
// first. Called on a copy of the config before each new connection.
func (d *DB) lastGoodFirst(_ context.Context, cc *pgx.ConnConfig) error {
	cc.ConnectTimeout = d.cfg.DBConnectTimeoutDur
	good, _ := d.lastGood.Load().(string)
	if good == "" {
		return nil
	}
	type target struct {
		host string
		port uint16
	}
	all := []target{{cc.Host, cc.Port}}
	for _, f := range cc.Fallbacks {
		all = append(all, target{f.Host, f.Port})
	}
	for i, t := range all {
		if net.JoinHostPort(t.host, strconv.Itoa(int(t.port))) != good || i == 0 {
			continue
		}
		all[0], all[i] = all[i], all[0]
		cc.Host, cc.Port = all[0].host, all[0].port
		fb := make([]*pgconn.FallbackConfig, 0, len(all)-1)
		for _, t := range all[1:] {
			fb = append(fb, &pgconn.FallbackConfig{Host: t.host, Port: t.port})
		}
		cc.Fallbacks = fb
		break
	}
	return nil
}

// remember records which server a new connection landed on, and logs it when
// that changes (that is a failover).
func (d *DB) remember(_ context.Context, c *pgx.Conn) error {
	addr := c.PgConn().Conn().RemoteAddr().String()
	if prev, _ := d.lastGood.Swap(addr).(string); prev != addr {
		if prev == "" {
			log.Printf("database %s: connected to primary %s", d.name, addr)
		} else {
			log.Printf("database %s: primary is now %s (was %s)", d.name, addr, prev)
		}
	}
	return nil
}

// Ready makes sure the tables exist, creating and seeding them the first time
// the database is reachable.
func (d *DB) Ready(ctx context.Context) error {
	if d.ready.Load() {
		return nil
	}
	d.setupMu.Lock()
	defer d.setupMu.Unlock()
	if d.ready.Load() {
		return nil
	}
	err := d.tx(ctx, func(tx *sql.Tx) error {
		// One node per database, but lock anyway so two copies of a node
		// started at once can't seed twice.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(4242)`); err != nil {
			return err
		}
		return d.setup(ctx, tx)
	})
	if err != nil {
		return err
	}
	d.ready.Store(true)
	log.Printf("database %s: ready", d.name)
	return nil
}

// Tx runs f in a transaction, committing if it returns nil. A gRPC status
// error returned by f is passed through as is; database errors become
// Unavailable (could not reach the database) or Internal.
func (d *DB) Tx(ctx context.Context, f func(*sql.Tx) error) error {
	if err := d.Ready(ctx); err != nil {
		return Status(err)
	}
	return Status(d.tx(ctx, f))
}

func (d *DB) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Status turns an error into a gRPC status error.
func Status(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if unreachable(err) {
		return status.Errorf(codes.Unavailable, "database unreachable: %v", err)
	}
	return status.Error(codes.Internal, err.Error())
}

// unreachable reports whether err means the database could not be used at all
// (no server, connection lost, server shutting down or read-only), as
// opposed to the query itself being wrong.
func unreachable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		c := pgErr.Code
		return strings.HasPrefix(c, "08") || strings.HasPrefix(c, "57") || c == "25006"
	}
	var netErr net.Error
	var connErr *pgconn.ConnectError
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &netErr) || errors.As(err, &connErr) || pgconn.SafeToRetry(err) ||
		strings.Contains(err.Error(), "conn closed")
}

// Stats reports the pool and which database server is the primary, for the
// status page.
func (d *DB) Stats(ctx context.Context) *pb.DBStatus {
	s := d.DB.Stats()
	out := &pb.DBStatus{
		Database: d.name,
		MaxOpen:  int32(s.MaxOpenConnections), Open: int32(s.OpenConnections),
		InUse: int32(s.InUse), Idle: int32(s.Idle),
		WaitCount: s.WaitCount, WaitMs: s.WaitDuration.Milliseconds(),
		MaxIdleClosed: s.MaxIdleClosed, MaxIdleTimeClosed: s.MaxIdleTimeClosed,
		MaxLifetimeClosed: s.MaxLifetimeClosed,
	}
	// Ask through the pool which server answered: that is the primary in use.
	qctx, cancel := context.WithTimeout(ctx, d.cfg.DBProbeTimeoutDur)
	defer cancel()
	var addr sql.NullString
	if err := d.QueryRowContext(qctx, `SELECT inet_server_addr()::text`).Scan(&addr); err != nil {
		out.Error = Status(err).Error()
	} else {
		out.ConnectedTo = strings.TrimSuffix(addr.String, "/32")
	}

	// Probe every server directly (outside the pool) for its role.
	out.Hosts = make([]*pb.DBHost, len(d.hosts))
	var wg sync.WaitGroup
	for i, hp := range d.hosts {
		wg.Go(func() { out.Hosts[i] = d.probe(ctx, hp) })
	}
	wg.Wait()
	return out
}

func (d *DB) probe(ctx context.Context, hostport string) *pb.DBHost {
	h := &pb.DBHost{Addr: hostport}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, "5432"
	}
	p, _ := strconv.Atoi(port)
	cc := d.connCfg.Copy()
	cc.Host, cc.Port, cc.Fallbacks, cc.ValidateConnect = host, uint16(p), nil, nil
	cc.ConnectTimeout = d.cfg.DBProbeTimeoutDur
	pctx, cancel := context.WithTimeout(ctx, d.cfg.DBProbeTimeoutDur)
	defer cancel()
	c, err := pgx.ConnectConfig(pctx, cc)
	if err != nil {
		h.Role, h.Error = "down", shortErr(err)
		return h
	}
	defer c.Close(context.Background())
	var standby bool
	if err := c.QueryRow(pctx, `SELECT pg_is_in_recovery()`).Scan(&standby); err != nil {
		h.Role, h.Error = "down", shortErr(err)
		return h
	}
	h.Role = "primary"
	if standby {
		h.Role = "standby (replica)"
	}
	return h
}

func shortErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer"
	case strings.Contains(err.Error(), "refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "no route"), strings.Contains(err.Error(), "unreachable"):
		return "host unreachable"
	}
	return err.Error()
}

// SeedOnce runs seed the first time only, so data the users delete later
// (a seeded announcement, say) is not recreated when the node restarts.
func SeedOnce(ctx context.Context, tx *sql.Tx, seed func() error) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS p4_meta (seeded_at timestamptz NOT NULL)`); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM p4_meta`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if err := seed(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO p4_meta VALUES (now())`)
	return err
}

// Health serves the Health gRPC service for a node: its database pool stats.
// It needs no login (see Public), so the status page works logged out.
func Health(d *DB) pb.HealthServer { return healthServer{d: d} }

// Public lists the methods every data node lets through without a token.
var Public = []string{pb.Health_DBStats_FullMethodName}

type healthServer struct {
	pb.UnimplementedHealthServer
	d *DB
}

func (h healthServer) DBStats(ctx context.Context, _ *pb.Empty) (*pb.DBStatus, error) {
	return h.d.Stats(ctx), nil
}
