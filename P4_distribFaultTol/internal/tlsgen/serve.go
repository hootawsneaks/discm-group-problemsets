package tlsgen

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// handshakeWait is how long a new connection has to send its first byte before
// it is dropped. Long enough for a slow network, short enough that a port
// scanner cannot pile up connections.
const handshakeWait = 15 * time.Second

// tlsRecordHandshake is the first byte of a TLS ClientHello (content type 22).
// A plain HTTP request starts with a method name instead, e.g. "G" of "GET".
const tlsRecordHandshake = 0x16

// ServeHTTPS serves h over TLS on addr, and answers plain HTTP on the *same*
// port with a redirect to the https:// URL.
//
// Without that redirect, opening http://host:8080 (an old bookmark, or just
// what a browser does when you type "host:8080") fails with nothing but a TLS
// handshake error in the log, which is a confusing way to lose a demo.
func ServeHTTPS(addr, certFile, keyFile string, h http.Handler) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	secure := newQueue(lis.Addr())
	plain := newQueue(lis.Addr())
	go func() {
		defer lis.Close()
		defer secure.Close()
		defer plain.Close()
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			// One goroutine per connection: reading the first byte must not
			// hold up anyone else's connection.
			go route(conn, secure, plain)
		}
	}()

	// Whichever server stops first ends the whole thing.
	errs := make(chan error, 2)
	go func() { errs <- (&http.Server{Handler: redirectToHTTPS()}).Serve(plain) }()
	go func() { errs <- (&http.Server{Handler: h}).ServeTLS(secure, certFile, keyFile) }()
	return <-errs
}

// route peeks at the first byte to tell TLS from plain HTTP, then hands the
// connection (first byte and all) to the matching server.
func route(conn net.Conn, secure, plain *queue) {
	if err := conn.SetReadDeadline(time.Now().Add(handshakeWait)); err != nil {
		conn.Close()
		return
	}
	first := make([]byte, 1)
	n, err := conn.Read(first)
	if n == 0 || err != nil {
		conn.Close()
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return
	}

	rewound := &rewoundConn{Conn: conn, unread: first[:n]}
	target := plain
	if first[0] == tlsRecordHandshake {
		target = secure
	}
	if !target.push(rewound) {
		conn.Close()
	}
}

func redirectToHTTPS() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host // includes the port, which is the one we are serving
		if host == "" {
			host = "localhost"
		}
		// Found, not a permanent redirect: a cached permanent one would be a
		// nuisance if this ever goes back to plain HTTP.
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusFound)
	})
}

// rewoundConn is a connection with its first byte put back, so the server that
// gets it sees the request from the start.
type rewoundConn struct {
	net.Conn
	unread []byte
}

func (c *rewoundConn) Read(p []byte) (int, error) {
	if len(c.unread) > 0 {
		n := copy(p, c.unread)
		c.unread = c.unread[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// queue is a net.Listener fed by hand, so one real listener can supply two
// http.Servers.
type queue struct {
	conns  chan net.Conn
	addr   net.Addr
	closed chan struct{}
	once   sync.Once
}

func newQueue(addr net.Addr) *queue {
	return &queue{conns: make(chan net.Conn), addr: addr, closed: make(chan struct{})}
}

// push hands a connection to the server, or reports false once closed.
func (q *queue) push(c net.Conn) bool {
	select {
	case q.conns <- c:
		return true
	case <-q.closed:
		return false
	}
}

func (q *queue) Accept() (net.Conn, error) {
	select {
	case c := <-q.conns:
		return c, nil
	case <-q.closed:
		return nil, net.ErrClosed
	}
}

func (q *queue) Close() error {
	q.once.Do(func() { close(q.closed) })
	return nil
}

func (q *queue) Addr() net.Addr { return q.addr }
