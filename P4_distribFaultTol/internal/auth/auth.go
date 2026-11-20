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
	users := map[string]User{
		"alice": {"Alice Santos", authz.RoleStudent, ""},
		"bob":   {"Bob Reyes", authz.RoleStudent, ""},
		"prof1": {"Prof. Cruz", authz.RoleFaculty, ""},
		"prof2": {"Prof. Lim", authz.RoleFaculty, ""},
	}
	for k, u := range users {
		h, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
		u.PasswordHash = string(h)
		users[k] = u
	}
	return data{Users: users, Sessions: map[string]Session{}}
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
	tok, c, err := authz.Sign(s.cfg.JWTSecret, r.Username, u.Name, u.Role, s.cfg.TTL)
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

func Run(cfg *config.Config) error {
	db, err := store.Open(filepath.Join(cfg.DataDir, "auth.json"), seed)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.AuthAddr))
	if err != nil {
		return err
	}
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret, pb.Auth_Login_FullMethodName)))
	pb.RegisterAuthServer(g, &server{cfg: cfg, db: db})
	fmt.Println("auth node listening on", lis.Addr())
	return g.Serve(lis)
}
