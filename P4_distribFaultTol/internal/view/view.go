// Package view is the view node (the V and C of MVC). It serves HTML over
// HTTPS and talks to the other nodes only through gRPC. If a node is down,
// only the pages that need it show an error; everything else keeps working.
package view

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/pool"
	"p4/internal/tlsgen"
	pb "p4/proto"
)

//go:embed templates/*.html
var templateFS embed.FS

const cookieName = "token"

type app struct {
	cfg      *config.Config
	auth     pb.AuthClient
	course   pb.CourseClient
	grade    pb.GradeClient
	announce pb.AnnounceClient
	pools    map[string]*pool.Pool
	pages    map[string]*template.Template
}

// page is what every template receives.
type page struct {
	User *authz.Claims
	Msg  string // success message (from ?msg=)
	Err  string // error message
	Data any
}

func Run(cfg *config.Config) error {
	a := &app{cfg: cfg, pages: map[string]*template.Template{}, pools: map[string]*pool.Pool{}}
	funcs := template.FuncMap{
		"when":     a.when,
		"inputDT":  a.inputDT,
		"ended":    func(s int64) bool { return s != 0 && time.Unix(s, 0).Before(time.Now()) },
		"years":    yearsLabel,
		"hasYear":  func(ys []int32, y int) bool { return slices.Contains(ys, int32(y)) },
		"category": categoryLabel,
	}
	for _, name := range []string{"login", "courses", "grades", "status", "announcements", "announcement"} {
		a.pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
	}

	// One pool of gRPC connections per node. grpc.NewClient connects
	// lazily, so the view starts even when other nodes are down, and a short
	// backoff means a node that comes back is picked up within seconds.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{
			BaseDelay: 200 * time.Millisecond, Multiplier: 1.6, MaxDelay: 2 * time.Second,
		}}),
	}
	for name, addr := range map[string]string{
		"auth": cfg.AuthAddr, "course": cfg.CourseAddr, "grade": cfg.GradeAddr, "announce": cfg.AnnounceAddr,
	} {
		p, err := pool.New(addr, cfg.PoolSize, opts...)
		if err != nil {
			return err
		}
		a.pools[name] = p
		defer p.Close()
	}
	a.auth = pb.NewAuthClient(a.pools["auth"])
	a.course = pb.NewCourseClient(a.pools["course"])
	a.grade = pb.NewGradeClient(a.pools["grade"])
	a.announce = pb.NewAnnounceClient(a.pools["announce"])

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /status", a.status)
	mux.HandleFunc("GET /courses", a.requireLogin(a.courses))
	mux.HandleFunc("POST /enroll", a.requireLogin(a.enroll))
	mux.HandleFunc("POST /drop", a.requireLogin(a.drop))
	mux.HandleFunc("GET /grades", a.requireLogin(a.grades))
	mux.HandleFunc("POST /grades", a.requireLogin(a.uploadGrade))
	mux.HandleFunc("GET /announcements", a.requireLogin(a.announcements))
	mux.HandleFunc("POST /announcements", a.requireLogin(a.createAnnouncement))
	mux.HandleFunc("GET /announcements/{id}", a.requireLogin(a.announcementPage))
	mux.HandleFunc("POST /announcements/{id}", a.requireLogin(a.updateAnnouncement))
	mux.HandleFunc("POST /announcements/{id}/delete", a.requireLogin(a.deleteAnnouncement))

	hosts, err := tlsgen.EnsureCert(cfg.TLSCert, cfg.TLSKey, cfg.TLSHosts)
	if err != nil {
		return fmt.Errorf("tls certificate: %w", err)
	}
	fmt.Printf("view node on %s (self-signed certificate for %v; the browser will warn once)\n",
		browserURL(cfg.ViewListen), hosts)
	fmt.Printf("%d gRPC connection(s) per node: auth %s, course %s, grade %s, announce %s\n",
		cfg.PoolSize, cfg.AuthAddr, cfg.CourseAddr, cfg.GradeAddr, cfg.AnnounceAddr)
	// ServeHTTPS also redirects plain http:// on this port, so typing
	// "localhost:8080" lands on the page instead of a TLS error.
	return tlsgen.ServeHTTPS(cfg.ViewListen, cfg.TLSCert, cfg.TLSKey, mux)
}

// ---- helpers ----

// user verifies the JWT cookie locally (no call to the auth node).
func (a *app) user(r *http.Request) (*authz.Claims, string) {
	ck, err := r.Cookie(cookieName)
	if err != nil {
		return nil, ""
	}
	c, err := authz.Parse(a.cfg.JWTSecret, ck.Value)
	if err != nil {
		return nil, ""
	}
	return c, ck.Value
}

type ctxKey struct{}

type session struct {
	user  *authz.Claims
	token string
}

func (a *app) requireLogin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, tok := a.user(r)
		if c == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, session{c, tok})))
	}
}

func sess(r *http.Request) session { return r.Context().Value(ctxKey{}).(session) }

// rpcCtx is a context for one gRPC call: token attached, bounded by rpc_timeout.
func (a *app) rpcCtx(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout)
	if s, ok := r.Context().Value(ctxKey{}).(session); ok {
		ctx = authz.WithToken(ctx, s.token)
	}
	return ctx, cancel
}

// down reports whether an error means the node could not be reached, as
// opposed to the node answering with a refusal.
func down(err error) bool {
	st, _ := status.FromError(err)
	return st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded
}

// errMsg turns a gRPC error into something to show the user.
func errMsg(node string, err error) string {
	if st, _ := status.FromError(err); st.Code() == codes.Unavailable &&
		strings.HasPrefix(st.Message(), "database unreachable") {
		// The node answered; it is its database that didn't.
		return fmt.Sprintf("The %s node is up but can't reach its database right now. If a database "+
			"server just went down, the replica takes over within seconds: try again shortly.", node)
	}
	if down(err) {
		return fmt.Sprintf("The %s node is down right now. This feature is unavailable; the rest of the site still works.", node)
	}
	st, _ := status.FromError(err)
	return st.Message()
}

func (a *app) render(w http.ResponseWriter, name string, p page) {
	if err := a.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		log.Println("render:", err)
	}
}

func redirectMsg(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// landing is where a logged-in user starts: the registrar has no courses or
// grades of their own, so they start on announcements.
func landing(role string) string {
	if role == authz.RoleRegistrar {
		return "/announcements"
	}
	return "/courses"
}

// browserURL turns a listen address like ":8080" into something clickable.
func browserURL(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "https://localhost" + addr
	}
	return "https://" + addr
}

// ---- controllers ----

func (a *app) home(w http.ResponseWriter, r *http.Request) {
	if c, _ := a.user(r); c != nil {
		http.Redirect(w, r, landing(c.Role), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) loginForm(w http.ResponseWriter, r *http.Request) {
	c, _ := a.user(r)
	a.render(w, "login", page{User: c, Msg: r.URL.Query().Get("msg")})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	res, err := a.auth.Login(ctx, &pb.LoginRequest{Username: r.FormValue("username"), Password: r.FormValue("password")})
	if err != nil {
		a.render(w, "login", page{Err: errMsg("auth", err)})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: res.Token, Path: "/", HttpOnly: true,
		Secure:   true, // the view is served over HTTPS
		SameSite: http.SameSiteLaxMode, MaxAge: int(a.cfg.TTL.Seconds()),
	})
	http.Redirect(w, r, landing(res.Role), http.StatusSeeOther)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	msg := "Logged out."
	if c, tok := a.user(r); c != nil {
		ctx, cancel := context.WithTimeout(authz.WithToken(r.Context(), tok), a.cfg.Timeout)
		defer cancel()
		if _, err := a.auth.Logout(ctx, &pb.Empty{}); err != nil {
			// The cookie is cleared anyway, but the other nodes will keep
			// honouring this token until it expires: say so plainly.
			msg = "Logged out of this browser, but " + errMsg("auth", err)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, Secure: true})
	redirectMsg(w, r, "/login", msg)
}

func (a *app) courses(w http.ResponseWriter, r *http.Request) {
	p := page{User: sess(r).user, Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("err")}
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	res, err := a.course.ListCourses(ctx, &pb.Empty{})
	if err != nil {
		p.Err = errMsg("course", err)
	} else {
		p.Data = res.Courses
	}
	a.render(w, "courses", p)
}

func (a *app) enroll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	code := r.FormValue("code")
	if _, err := a.course.Enroll(ctx, &pb.EnrollRequest{Code: code}); err != nil {
		http.Redirect(w, r, "/courses?err="+url.QueryEscape(errMsg("course", err)), http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, "/courses", "Enrolled in "+code+".")
}

func (a *app) drop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	code := r.FormValue("code")
	if _, err := a.course.Drop(ctx, &pb.EnrollRequest{Code: code}); err != nil {
		http.Redirect(w, r, "/courses?err="+url.QueryEscape(errMsg("course", err)), http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, "/courses", "Dropped "+code+".")
}

func (a *app) grades(w http.ResponseWriter, r *http.Request) {
	s := sess(r)
	p := page{User: s.user, Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("err")}
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	var res *pb.GradeList
	var err error
	if s.user.Role == authz.RoleFaculty {
		res, err = a.grade.UploadedGrades(ctx, &pb.Empty{})
	} else {
		res, err = a.grade.MyGrades(ctx, &pb.Empty{})
	}
	if err != nil {
		p.Err = errMsg("grade", err)
	} else {
		p.Data = res.Grades
	}
	a.render(w, "grades", p)
}

func (a *app) uploadGrade(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.rpcCtx(r)
	defer cancel()
	_, err := a.grade.UploadGrade(ctx, &pb.GradeEntry{
		Student: r.FormValue("student"), Course: r.FormValue("course"),
		Term: r.FormValue("term"), Grade: r.FormValue("grade"),
	})
	if err != nil {
		http.Redirect(w, r, "/grades?err="+url.QueryEscape(errMsg("grade", err)), http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, "/grades", "Grade saved.")
}

// ---- status page ----

// nodeStatus is one row of the status page.
type nodeStatus struct {
	Name     string
	Addr     string
	Up       bool
	Features string
	Conns    int    // gRPC connections in this node's pool
	Ready    int    // how many of them are connected right now
	Detail   string // why it looks down
	DB       *pb.DBStatus
}

type statusPage struct {
	Nodes   []nodeStatus
	DBHosts []*pb.DBHost // every database server and its role, as a node sees it
}

// status asks every node for its health and database pool, at the same time,
// and says which features are available. It needs no login, so you can see
// what is up before trying to log in.
func (a *app) status(w http.ResponseWriter, r *http.Request) {
	c, _ := a.user(r)
	nodes := []nodeStatus{
		{Name: "auth", Features: "login, logout, session revocation"},
		{Name: "course", Features: "view courses, enroll, drop"},
		{Name: "grade", Features: "view grades, upload grades"},
		{Name: "announce", Features: "registrar announcements (create, read, edit, delete)"},
	}
	var wg sync.WaitGroup
	for i := range nodes {
		n := &nodes[i]
		p := a.pools[n.Name]
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout)
			defer cancel()
			st, err := pb.NewHealthClient(p).DBStats(ctx, &pb.Empty{})
			n.Addr, n.Conns, n.Ready = p.Addr(), p.Size(), p.Ready()
			n.Up = !down(err)
			if err != nil {
				s, _ := status.FromError(err)
				n.Detail = s.Code().String()
				return
			}
			n.DB = st
		})
	}
	wg.Wait()

	sp := statusPage{Nodes: append([]nodeStatus{{
		Name: "view", Addr: a.cfg.ViewListen, Up: true, Features: "pages and controllers (this node)",
	}}, nodes...)}
	for _, n := range nodes {
		if n.DB != nil && len(n.DB.Hosts) > 0 {
			sp.DBHosts = n.DB.Hosts
			break
		}
	}
	a.render(w, "status", page{User: c, Data: sp})
}
