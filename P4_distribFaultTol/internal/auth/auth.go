// Package auth is the auth node: login (issues a JWT), logout (revokes it),
// and a record of every session issued. Users and sessions live in the auth
// node's own database.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/db"
	pb "p4/proto"
)

type user struct {
	username, name, role string
	year                 int // students only: year level 1-4
}

// Demo accounts; every password is "password".
var demoUsers = []user{
	{"alice", "Alice Santos", authz.RoleStudent, 1},
	{"bob", "Bob Reyes", authz.RoleStudent, 3},
	{"prof1", "Prof. Cruz", authz.RoleFaculty, 0},
	{"prof2", "Prof. Lim", authz.RoleFaculty, 0},
	{"registrar", "Office of the Registrar", authz.RoleRegistrar, 0},
}

func setup(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			username      text PRIMARY KEY,
			name          text NOT NULL,
			role          text NOT NULL,
			year          int  NOT NULL DEFAULT 0,
			password_hash text NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sessions (
			jti        text PRIMARY KEY,
			username   text NOT NULL,
			issued_at  timestamptz NOT NULL,
			expires_at timestamptz NOT NULL,
			logged_out boolean NOT NULL DEFAULT false
		)`)
	if err != nil {
		return err
	}
	return db.SeedOnce(ctx, tx, func() error {
		for _, u := range demoUsers {
			h, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
				u.username, u.name, u.role, u.year, string(h)); err != nil {
				return err
			}
		}
		return nil
	})
}

type server struct {
	pb.UnimplementedAuthServer
	cfg *config.Config
	db  *db.DB
}

func (s *server) Login(ctx context.Context, r *pb.LoginRequest) (*pb.LoginReply, error) {
	var u user
	var hash string
	var tok string
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT name, role, year, password_hash FROM users WHERE username = $1`, r.Username).
			Scan(&u.name, &u.role, &u.year, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			return status.Error(codes.Unauthenticated, "wrong username or password")
		}
		if err != nil {
			return err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.Password)) != nil {
			return status.Error(codes.Unauthenticated, "wrong username or password")
		}
		var c *authz.Claims
		tok, c, err = authz.Sign(s.cfg.JWTSecret, r.Username, u.name, u.role, u.year, s.cfg.TTL)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions VALUES ($1, $2, $3, $4, false)`,
			c.ID, r.Username, c.IssuedAt.Time, c.ExpiresAt.Time)
		if err == nil {
			log.Printf("login %s (%s) session %s", r.Username, u.role, c.ID)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &pb.LoginReply{Token: tok, Username: r.Username, Role: u.role, Name: u.name}, nil
}

func (s *server) Logout(ctx context.Context, _ *pb.Empty) (*pb.Empty, error) {
	c := authz.FromContext(ctx)
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		// Upsert: a token signed before this database existed is still
		// recorded as logged out.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions VALUES ($1, $2, $3, $4, true)
			ON CONFLICT (jti) DO UPDATE SET logged_out = true`,
			c.ID, c.Username(), c.IssuedAt.Time, c.ExpiresAt.Time)
		return err
	})
	if err != nil {
		return nil, err
	}
	log.Printf("logout %s session %s", c.Username(), c.ID)
	return &pb.Empty{}, nil
}

// SessionStatus tells another node whether a session is still live. The course,
// grade and announce nodes call this so a logout reaches them too. A logged-out
// token is still a validly signed one, so the interceptor lets the call in and
// the answer here is what matters.
func (s *server) SessionStatus(ctx context.Context, r *pb.SessionQuery) (*pb.SessionState, error) {
	var out pb.SessionState
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT logged_out FROM sessions WHERE jti = $1`, r.GetJti()).Scan(&out.Revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		out.Known = err == nil
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func Run(cfg *config.Config) error {
	d, err := db.Open(cfg, "auth", setup)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.AuthAddr))
	if err != nil {
		return err
	}
	// The auth node is the source of truth for revocation, so it has no
	// Revoker of its own; Login and the health check need no token.
	public := append([]string{pb.Auth_Login_FullMethodName}, db.Public...)
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret, nil, public...)))
	pb.RegisterAuthServer(g, &server{cfg: cfg, db: d})
	pb.RegisterHealthServer(g, db.Health(d))
	fmt.Println("auth node listening on", lis.Addr())
	return g.Serve(lis)
}
