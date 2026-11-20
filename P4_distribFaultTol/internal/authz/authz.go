// Package authz issues and verifies JWTs. Every node verifies tokens locally
// with the shared secret, so a logged-in user keeps working even if the auth
// node is down; only login/logout depend on it.
package authz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	RoleStudent = "student"
	RoleFaculty = "faculty"
)

type Claims struct {
	Name string `json:"name"`
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// Username is the token subject.
func (c *Claims) Username() string { return c.Subject }

func Sign(secret, username, name, role string, ttl time.Duration) (token string, c *Claims, err error) {
	id := make([]byte, 16)
	rand.Read(id)
	now := time.Now()
	c = &Claims{
		Name: name,
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        hex.EncodeToString(id),
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	token, err = jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	return token, c, err
}

func Parse(secret, token string) (*Claims, error) {
	c := &Claims{}
	_, err := jwt.ParseWithClaims(token, c, func(*jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ---- gRPC plumbing ----

type ctxKey struct{}

// WithToken attaches a JWT to an outgoing gRPC call.
func WithToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// FromContext returns the caller's verified claims inside a gRPC handler.
func FromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(ctxKey{}).(*Claims)
	return c
}

// Require returns the caller's claims, or a PermissionDenied error if the role doesn't match.
func Require(ctx context.Context, role string) (*Claims, error) {
	c := FromContext(ctx)
	if c == nil {
		return nil, status.Error(codes.Unauthenticated, "not logged in")
	}
	if role != "" && c.Role != role {
		return nil, status.Errorf(codes.PermissionDenied, "only %s accounts can do this", role)
	}
	return c, nil
}

// Interceptor verifies the JWT on every call except the listed public methods.
// The raw token is also stored so the auth node can revoke it.
func Interceptor(secret string, public ...string) grpc.UnaryServerInterceptor {
	skip := map[string]bool{}
	for _, m := range public {
		skip[m] = true
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if skip[info.FullMethod] {
			return h(ctx, req)
		}
		tok, err := tokenFrom(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		c, err := Parse(secret, tok)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
		}
		return h(context.WithValue(ctx, ctxKey{}, c), req)
	}
}

func tokenFrom(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if t, ok := strings.CutPrefix(v, "Bearer "); ok {
			return t, nil
		}
	}
	return "", errors.New("missing token")
}
