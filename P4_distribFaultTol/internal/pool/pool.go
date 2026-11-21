// Package pool keeps several gRPC connections open to one node and hands them
// out round-robin (bonus 3: connection pooling).
//
// One gRPC connection multiplexes many calls over a single HTTP/2 stream, which
// is fine until the per-connection stream limit or a slow call starts queueing
// others behind it. A small pool spreads the view node's calls over several
// sockets instead, and gives a failed node more than one path to recover on.
package pool

import (
	"context"
	"fmt"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

type Pool struct {
	conns []*grpc.ClientConn
	next  atomic.Uint64
	addr  string
}

// New opens size lazy connections to addr. grpc.NewClient does not connect
// until the first call, so this works with the node still down.
func New(addr string, size int, opts ...grpc.DialOption) (*Pool, error) {
	if size < 1 {
		size = 1
	}
	p := &Pool{addr: addr, conns: make([]*grpc.ClientConn, 0, size)}
	for i := 0; i < size; i++ {
		conn, err := grpc.NewClient(addr, opts...)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("%s: %w", addr, err)
		}
		p.conns = append(p.conns, conn)
	}
	return p, nil
}

// Conn returns a connection to use for the next call. It is safe for
// concurrent use.
//
// It round-robins, but skips connections that are currently broken. After a
// node restarts, only the connections that happened to be used during the
// outage are in TRANSIENT_FAILURE, and a call on one of those fails
// immediately even though the node is back. Handing them out blindly would
// make a recovered node look intermittently down, on a rotating share of
// requests, until every connection's backoff had elapsed.
func (p *Pool) Conn() grpc.ClientConnInterface {
	start := p.next.Add(1) - 1
	n := uint64(len(p.conns))

	for i := uint64(0); i < n; i++ {
		c := p.conns[(start+i)%n]
		switch c.GetState() {
		case connectivity.TransientFailure, connectivity.Shutdown:
			continue // broken: a call on this would fail right away
		default:
			// Ready, Idle or Connecting. An idle one is left to be dialled by
			// the call itself; warming it up here while the node is down would
			// break it too, leaving nothing healthy for when the node returns.
			return c
		}
	}
	// Everything is broken. Return the plain round-robin pick so the call
	// reports the real problem instead of hiding it.
	return p.conns[start%n]
}

// Size is how many connections the pool holds.
func (p *Pool) Size() int { return len(p.conns) }

// Addr is the node the pool points at.
func (p *Pool) Addr() string { return p.addr }

// Ready reports how many of the pool's connections are currently connected.
// It is only used to show something useful on the status page.
func (p *Pool) Ready() int {
	n := 0
	for _, c := range p.conns {
		if c.GetState() == connectivity.Ready {
			n++
		}
	}
	return n
}

func (p *Pool) Close() {
	for _, c := range p.conns {
		c.Close()
	}
}

// ---- making the pool usable as a generated client's connection ----

// Invoke sends a unary call on the next connection in the pool, so a *Pool can
// be passed straight to pb.NewAuthClient and friends.
func (p *Pool) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	return p.Conn().Invoke(ctx, method, args, reply, opts...)
}

// NewStream does the same for streaming calls.
func (p *Pool) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return p.Conn().NewStream(ctx, desc, method, opts...)
}
