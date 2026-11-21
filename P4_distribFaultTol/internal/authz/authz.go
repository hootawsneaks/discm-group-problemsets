// Package authz issues and verifies JWTs. Every node verifies tokens locally
// with the shared secret, so a logged-in user keeps working even if the auth
// node is down; only login/logout depend on it.
//
// Verifying locally has one catch: a signed token stays valid until it expires,
// even after the user logs out. So nodes also ask the auth node whether a
// session was revoked (see RemoteRevoker) — and let the call through when the
// auth node cannot be reached, which keeps the fault tolerance above.
package authz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "p4/proto"
)

const (
	RoleStudent   = "student"
	RoleFaculty   = "faculty"
	RoleRegistrar = "registrar"
)

type Claims struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// Year is a student's year level (1-4), 0 for everyone else. It travels in
	// the token so a node can act on it without asking the auth node.
	Year int `json:"year,omitempty"`
	jwt.RegisteredClaims
}

// Username is the token subject.
func (c *Claims) Username() string { return c.Subject }

func Sign(secret, username, name, role string, year int, ttl time.Duration) (token string, c *Claims, err error) {
	id := make([]byte, 16)
	rand.Read(id)
	now := time.Now()
	c = &Claims{
		Name: name,
		Role: role,
		Year: year,
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
type tokenKey struct{}

// WithToken attaches a JWT to an outgoing gRPC call.
func WithToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// FromContext returns the caller's verified claims inside a gRPC handler.
func FromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(ctxKey{}).(*Claims)
	return c
}

// TokenFromContext returns the raw JWT the caller sent, so a handler can pass
// it on to another node.
func TokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(tokenKey{}).(string)
	return t
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

// Revoker reports whether the session with this token id has been logged out.
// An error means "could not tell", and the interceptor then allows the call.
type Revoker func(ctx context.Context, token, jti string) (revoked bool, err error)

// Interceptor verifies the JWT on every call except the listed public methods.
// The verified claims and the raw token are put on the context. When rev is not
// nil it is also asked whether the session was revoked.
func Interceptor(secret string, rev Revoker, public ...string) grpc.UnaryServerInterceptor {
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
		if rev != nil {
			// A failure here is deliberately not fatal: see the package comment.
			if revoked, err := rev(ctx, tok, c.ID); err == nil && revoked {
				return nil, status.Error(codes.Unauthenticated, "this session was logged out")
			}
		}
		ctx = context.WithValue(ctx, ctxKey{}, c)
		ctx = context.WithValue(ctx, tokenKey{}, tok)
		return h(ctx, req)
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

// ---- asking the auth node about a session ----

// unreachableCooldown is how long a node leaves the auth node alone after it
// fails to answer. It is deliberately not configurable: it only has to be long
// enough that a dead auth node is not re-dialled on every single request, and
// short enough that a logout is honoured again soon after auth comes back.
const unreachableCooldown = 2 * time.Second

type revocations struct {
	addr    string
	timeout time.Duration
	ttl     time.Duration

	mu       sync.Mutex
	client   pb.AuthClient
	live     map[string]time.Time // jti -> when we last confirmed it is live
	revoked  map[string]bool      // jti -> logged out (permanent, so never expires)
	downTill time.Time            // skip asking until then: auth is not answering
	lastWarn time.Time
}

// RemoteRevoker returns a Revoker that asks the auth node at addr whether a
// session is still live. Logouts are remembered for good. If the auth node
// cannot be reached the answer is an error, which the interceptor treats as
// "not revoked" — so taking the auth node down stops logins, not everything.
//
// ttl is how long a "this session is live" answer is reused before asking
// again. Any ttl above zero is a window in which a logout elsewhere is not
// noticed yet, so the default is zero: ask every time. One extra round trip per
// request is cheap, and logging out working everywhere immediately is the point.
//
// timeout must be a small fraction of the caller's own deadline. A host that
// is switched off does not refuse connections, it swallows them, so this call
// would otherwise sit there until it times out and eat the whole request
// budget — making *this* node look down too. After one such failure the auth
// node is left alone for ttl, so only the first request pays the wait.
func RemoteRevoker(addr string, timeout, ttl time.Duration) Revoker {
	r := &revocations{
		addr: addr, timeout: timeout, ttl: ttl,
		live: map[string]time.Time{}, revoked: map[string]bool{},
	}
	return r.check
}

func (r *revocations) check(ctx context.Context, token, jti string) (bool, error) {
	if jti == "" {
		return false, nil
	}
	r.mu.Lock()
	if r.revoked[jti] {
		r.mu.Unlock()
		return true, nil
	}
	if seen, ok := r.live[jti]; ok && time.Since(seen) < r.ttl {
		r.mu.Unlock()
		return false, nil
	}
	if time.Now().Before(r.downTill) {
		// The auth node just failed to answer. Don't make every request wait
		// for it again; carry on as if the session were live.
		r.mu.Unlock()
		return false, nil
	}
	client, err := r.dial()
	r.mu.Unlock()
	if err != nil {
		return false, err
	}

	callCtx, cancel := context.WithTimeout(WithToken(context.WithoutCancel(ctx), token), r.timeout)
	defer cancel()
	state, err := client.SessionStatus(callCtx, &pb.SessionQuery{Jti: jti})
	if err != nil {
		r.mu.Lock()
		r.downTill = time.Now().Add(unreachableCooldown)
		r.mu.Unlock()
		r.warn(err)
		return false, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.downTill = time.Time{} // it answered, so start asking again
	// A session the auth node has no record of (its data was reset, or the
	// token came from another auth node) is left alone: the signature and the
	// expiry already vouch for it.
	if state.GetRevoked() {
		r.revoked[jti] = true
		delete(r.live, jti)
		return true, nil
	}
	if len(r.live) > 4096 { // keep the cache from growing without bound
		r.live = map[string]time.Time{}
	}
	r.live[jti] = time.Now()
	return false, nil
}

// dial connects lazily, so a node starts even with the auth node down. Called
// with r.mu held.
func (r *revocations) dial() (pb.AuthClient, error) {
	if r.client != nil {
		return r.client, nil
	}
	conn, err := grpc.NewClient(r.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	r.client = pb.NewAuthClient(conn)
	return r.client, nil
}

// warn logs at most once every 10 seconds, so a node that runs for a while
// with the auth node down doesn't fill the log.
func (r *revocations) warn(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastWarn) < 10*time.Second {
		return
	}
	r.lastWarn = time.Now()
	log.Printf("auth node at %s unreachable (%v): allowing logged-in sessions, "+
		"so a logout elsewhere is not noticed until it comes back", r.addr, err)
}
