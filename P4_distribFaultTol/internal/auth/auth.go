// Package auth is the auth node: login (issues a JWT), logout (revokes it),
// and a record of every session issued.
package auth

import (
	"context"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/store"
	pb "p4/proto"
)

type User struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	Year         int    `json:"year,omitempty"` // students only: year level 1-4
	PasswordHash string `json:"password_hash"`
}

type Session struct {
	Username  string    `json:"username"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	LoggedOut bool      `json:"logged_out"`
}

type data struct {
	Users    map[string]User    `json:"users"`
	Sessions map[string]Session `json:"sessions"` // keyed by token ID (jti)
}

// Demo accounts; every password is "password".
func seed() data {
	users := seedUsers()
	for k, u := range users {
		h, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
		u.PasswordHash = string(h)
		users[k] = u
	}
	return data{Users: users, Sessions: map[string]Session{}}
}

func seedUsers() map[string]User {
	return map[string]User{
		"alice":     {"Alice Santos", authz.RoleStudent, 1, ""},
		"bob":       {"Bob Reyes", authz.RoleStudent, 3, ""},
		"prof1":     {"Prof. Cruz", authz.RoleFaculty, 0, ""},
		"prof2":     {"Prof. Lim", authz.RoleFaculty, 0, ""},
		"registrar": {"Office of the Registrar", authz.RoleRegistrar, 0, ""},
	}
}

// addMissingSeedUsers brings a data file written by an older version up to
// date: demo accounts it lacks are added and students get their year level.
// Existing passwords and sessions are left alone.
func addMissingSeedUsers(d *data) error {
	for k, want := range seedUsers() {
		u, ok := d.Users[k]
		if !ok {
			h, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			want.PasswordHash = string(h)
			d.Users[k] = want
			continue
		}
		if u.Role == authz.RoleStudent && u.Year == 0 {
			u.Year = want.Year
			d.Users[k] = u
		}
	}
	return nil
}

type server struct {
	pb.UnimplementedAuthServer
	cfg *config.Config
	db  *store.Store[data]
}

func (s *server) Login(_ context.Context, r *pb.LoginRequest) (*pb.LoginReply, error) {
	var u User
	var ok bool
	s.db.Read(func(d *data) { u, ok = d.Users[r.Username] })
	if !ok || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(r.Password)) != nil {
		return nil, status.Error(codes.Unauthenticated, "wrong username or password")
	}
	tok, c, err := authz.Sign(s.cfg.JWTSecret, r.Username, u.Name, u.Role, u.Year, s.cfg.TTL)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	err = s.db.Update(func(d *data) error {
		d.Sessions[c.ID] = Session{r.Username, c.IssuedAt.Time, c.ExpiresAt.Time, false}
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	log.Printf("login %s (%s) session %s", r.Username, u.Role, c.ID)
	return &pb.LoginReply{Token: tok, Username: r.Username, Role: u.Role, Name: u.Name}, nil
}

func (s *server) Logout(ctx context.Context, _ *pb.Empty) (*pb.Empty, error) {
	c := authz.FromContext(ctx)
	err := s.db.Update(func(d *data) error {
		sess := d.Sessions[c.ID]
		sess.Username, sess.LoggedOut = c.Username(), true
		d.Sessions[c.ID] = sess
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	log.Printf("logout %s session %s", c.Username(), c.ID)
	return &pb.Empty{}, nil
}

// SessionStatus tells another node whether a session is still live. The course
// and grade nodes call this so a logout reaches them too. A logged-out token is
// still a validly signed one, so the interceptor lets the call in and the
// answer here is what matters.
func (s *server) SessionStatus(_ context.Context, r *pb.SessionQuery) (*pb.SessionState, error) {
	var sess Session
	var known bool
	s.db.Read(func(d *data) { sess, known = d.Sessions[r.GetJti()] })
	return &pb.SessionState{Revoked: known && sess.LoggedOut, Known: known}, nil
}

func Run(cfg *config.Config) error {
	db, err := store.Open(filepath.Join(cfg.DataDir, "auth.json"), seed)
	if err != nil {
		return err
	}
	if err := db.Update(addMissingSeedUsers); err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.AuthAddr))
	if err != nil {
		return err
	}
	// The auth node is the source of truth for revocation, so it has no
	// Revoker of its own; Login is the one method that needs no token.
	g := grpc.NewServer(grpc.UnaryInterceptor(
		authz.Interceptor(cfg.JWTSecret, nil, pb.Auth_Login_FullMethodName)))
	pb.RegisterAuthServer(g, &server{cfg: cfg, db: db})
	fmt.Println("auth node listening on", lis.Addr())
	return g.Serve(lis)
}
