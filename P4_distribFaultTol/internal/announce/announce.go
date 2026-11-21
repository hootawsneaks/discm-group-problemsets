// Package announce is the announce node: registrar announcements such as the
// enrollment schedule per year level, enrollment confirmation and payment
// deadlines. It is its own object with its own full CRUD on its own node, and
// it needs no other node to answer: a student's year level comes from their
// token, so even with the auth node down, logged-in users can read it.
// Announcements live in the announce node's own database.
package announce

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/db"
	pb "p4/proto"
)

// Categories an announcement can have.
var Categories = []string{"schedule", "confirmation", "payment", "general"}

type Announcement struct {
	ID         string    `json:"id"`
	Category   string    `json:"category"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	YearLevels []int     `json:"year_levels"` // empty = everyone
	StartsAt   time.Time `json:"starts_at"`   // zero = no start
	EndsAt     time.Time `json:"ends_at"`     // zero = no end
	Pinned     bool      `json:"pinned"`
	Author     string    `json:"author"`
	AuthorName string    `json:"author_name"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const schema = `
	CREATE TABLE IF NOT EXISTS announcements (
		id          text PRIMARY KEY,
		category    text NOT NULL,
		title       text NOT NULL,
		body        text NOT NULL,
		year_levels text NOT NULL DEFAULT '', -- e.g. "1,2"; empty = everyone
		starts_at   timestamptz,
		ends_at     timestamptz,
		pinned      boolean NOT NULL DEFAULT false,
		author      text NOT NULL,
		author_name text NOT NULL,
		created_at  timestamptz NOT NULL,
		updated_at  timestamptz NOT NULL
	)`

const columns = `id, category, title, body, year_levels, starts_at, ends_at, pinned,
	author, author_name, created_at, updated_at`

func setup(loc *time.Location) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
		return db.SeedOnce(ctx, tx, func() error {
			for _, a := range seed(loc) {
				if err := insert(ctx, tx, a); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

func seed(loc *time.Location) []Announcement {
	now := time.Now().In(loc)
	day := func(d, h int) time.Time {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, loc)
		return t.AddDate(0, 0, d)
	}
	mk := func(id, cat, title, body string, years []int, start, end time.Time, pinned bool) Announcement {
		return Announcement{
			ID: id, Category: cat, Title: title, Body: body, YearLevels: years,
			StartsAt: start, EndsAt: end, Pinned: pinned,
			Author: "registrar", AuthorName: "Office of the Registrar",
			CreatedAt: now, UpdatedAt: now,
		}
	}
	return []Announcement{
		mk("seed-sched-4", "schedule", "Enrollment schedule: 4th year",
			"4th year students may enroll during this window. Graduating students should settle any holds first.",
			[]int{4}, day(1, 9), day(2, 17), false),
		mk("seed-sched-3", "schedule", "Enrollment schedule: 3rd year",
			"3rd year students may enroll during this window.",
			[]int{3}, day(3, 9), day(4, 17), false),
		mk("seed-sched-12", "schedule", "Enrollment schedule: 1st and 2nd year",
			"1st and 2nd year students may enroll during this window. Block sections are pre-loaded for 1st years.",
			[]int{1, 2}, day(5, 9), day(6, 17), false),
		mk("seed-confirm", "confirmation", "Confirm your enrollment",
			"Review and confirm your enrolled courses before the deadline. Unconfirmed slots will be released to other students.",
			nil, time.Time{}, day(8, 17), true),
		mk("seed-payment", "payment", "Tuition payment deadline",
			"Tuition for the coming term must be paid by the deadline. Late payments incur a PHP 500 fee.",
			nil, time.Time{}, day(12, 17), true),
	}
}

// ---- database rows ----

func joinYears(ys []int) string {
	parts := make([]string, len(ys))
	for i, y := range ys {
		parts[i] = strconv.Itoa(y)
	}
	return strings.Join(parts, ",")
}

func splitYears(s string) []int {
	var ys []int
	for _, p := range strings.Split(s, ",") {
		if y, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			ys = append(ys, y)
		}
	}
	return ys
}

func nullTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: !t.IsZero()} }

func insert(ctx context.Context, tx *sql.Tx, a Announcement) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO announcements (`+columns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT DO NOTHING`,
		a.ID, a.Category, a.Title, a.Body, joinYears(a.YearLevels), nullTime(a.StartsAt), nullTime(a.EndsAt),
		a.Pinned, a.Author, a.AuthorName, a.CreatedAt, a.UpdatedAt)
	return err
}

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Announcement, error) {
	var a Announcement
	var years string
	var start, end sql.NullTime
	err := row.Scan(&a.ID, &a.Category, &a.Title, &a.Body, &years, &start, &end, &a.Pinned,
		&a.Author, &a.AuthorName, &a.CreatedAt, &a.UpdatedAt)
	a.YearLevels, a.StartsAt, a.EndsAt = splitYears(years), start.Time, end.Time
	return a, err
}

type server struct {
	pb.UnimplementedAnnounceServer
	db *db.DB
}

// visible reports whether the caller may see a: everyone except students sees
// everything; a student sees what is for everyone or for their year level.
func visible(c *authz.Claims, a Announcement) bool {
	if c.Role != authz.RoleStudent || len(a.YearLevels) == 0 {
		return true
	}
	return slices.Contains(a.YearLevels, c.Year)
}

func toPB(a Announcement) *pb.Announcement {
	years := make([]int32, len(a.YearLevels))
	for i, y := range a.YearLevels {
		years[i] = int32(y)
	}
	return &pb.Announcement{
		Id: a.ID, Category: a.Category, Title: a.Title, Body: a.Body, YearLevels: years,
		StartsAt: unix(a.StartsAt), EndsAt: unix(a.EndsAt), Pinned: a.Pinned,
		Author: a.Author, AuthorName: a.AuthorName,
		CreatedAt: unix(a.CreatedAt), UpdatedAt: unix(a.UpdatedAt),
	}
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0)
}

// fromRequest validates the client-supplied fields of r.
func fromRequest(r *pb.Announcement) (Announcement, error) {
	a := Announcement{
		Category: strings.TrimSpace(r.Category),
		Title:    strings.TrimSpace(r.Title),
		Body:     strings.TrimSpace(r.Body),
		StartsAt: fromUnix(r.StartsAt),
		EndsAt:   fromUnix(r.EndsAt),
		Pinned:   r.Pinned,
	}
	if !slices.Contains(Categories, a.Category) {
		return a, status.Errorf(codes.InvalidArgument, "category must be one of %s", strings.Join(Categories, ", "))
	}
	if a.Title == "" || a.Body == "" {
		return a, status.Error(codes.InvalidArgument, "title and body are required")
	}
	if !a.StartsAt.IsZero() && !a.EndsAt.IsZero() && a.EndsAt.Before(a.StartsAt) {
		return a, status.Error(codes.InvalidArgument, "the end date is before the start date")
	}
	for _, y := range r.YearLevels {
		if y < 1 || y > 4 {
			return a, status.Errorf(codes.InvalidArgument, "year level %d is not 1-4", y)
		}
		if !slices.Contains(a.YearLevels, int(y)) {
			a.YearLevels = append(a.YearLevels, int(y))
		}
	}
	slices.Sort(a.YearLevels)
	return a, nil
}

// order puts pinned first, then what is still running or upcoming, then what
// has ended; within each, the soonest deadline first, then the newest post.
func order(now time.Time) func(a, b Announcement) int {
	ended := func(a Announcement) bool { return !a.EndsAt.IsZero() && a.EndsAt.Before(now) }
	rank := func(a Announcement) int {
		switch {
		case a.Pinned && !ended(a):
			return 0
		case !ended(a):
			return 1
		}
		return 2
	}
	return func(a, b Announcement) int {
		if c := cmp.Compare(rank(a), rank(b)); c != 0 {
			return c
		}
		if !a.EndsAt.Equal(b.EndsAt) && !a.EndsAt.IsZero() && !b.EndsAt.IsZero() {
			return a.EndsAt.Compare(b.EndsAt)
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	}
}

func (s *server) ListAnnouncements(ctx context.Context, _ *pb.Empty) (*pb.AnnouncementList, error) {
	c, err := authz.Require(ctx, "")
	if err != nil {
		return nil, err
	}
	var list []Announcement
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM announcements`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scan(rows)
			if err != nil {
				return err
			}
			if visible(c, a) {
				list = append(list, a)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(list, order(time.Now()))
	out := &pb.AnnouncementList{}
	for _, a := range list {
		out.Announcements = append(out.Announcements, toPB(a))
	}
	return out, nil
}

func (s *server) GetAnnouncement(ctx context.Context, r *pb.AnnouncementId) (*pb.Announcement, error) {
	c, err := authz.Require(ctx, "")
	if err != nil {
		return nil, err
	}
	var a Announcement
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		a, err = scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM announcements WHERE id = $1`, r.Id))
		if errors.Is(err, sql.ErrNoRows) {
			return status.Errorf(codes.NotFound, "no announcement %q", r.Id)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// An announcement for another year level is reported as missing, so a
	// student cannot tell it exists.
	if !visible(c, a) {
		return nil, status.Errorf(codes.NotFound, "no announcement %q", r.Id)
	}
	return toPB(a), nil
}

func (s *server) CreateAnnouncement(ctx context.Context, r *pb.Announcement) (*pb.Announcement, error) {
	c, err := authz.Require(ctx, authz.RoleRegistrar)
	if err != nil {
		return nil, err
	}
	a, err := fromRequest(r)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 8)
	rand.Read(id)
	now := time.Now()
	a.ID, a.Author, a.AuthorName, a.CreatedAt, a.UpdatedAt = hex.EncodeToString(id), c.Username(), c.Name, now, now
	if err := s.db.Tx(ctx, func(tx *sql.Tx) error { return insert(ctx, tx, a) }); err != nil {
		return nil, err
	}
	log.Printf("%s posted announcement %s %q", c.Username(), a.ID, a.Title)
	return toPB(a), nil
}

func (s *server) UpdateAnnouncement(ctx context.Context, r *pb.Announcement) (*pb.Announcement, error) {
	c, err := authz.Require(ctx, authz.RoleRegistrar)
	if err != nil {
		return nil, err
	}
	a, err := fromRequest(r)
	if err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		// The registrar is an office: any registrar account may edit any
		// announcement. Who first posted it and when are kept.
		a.ID, a.UpdatedAt = r.Id, time.Now()
		err := tx.QueryRowContext(ctx, `
			UPDATE announcements SET category = $2, title = $3, body = $4, year_levels = $5,
				starts_at = $6, ends_at = $7, pinned = $8, updated_at = $9
			WHERE id = $1 RETURNING author, author_name, created_at`,
			a.ID, a.Category, a.Title, a.Body, joinYears(a.YearLevels), nullTime(a.StartsAt), nullTime(a.EndsAt),
			a.Pinned, a.UpdatedAt).Scan(&a.Author, &a.AuthorName, &a.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return status.Errorf(codes.NotFound, "no announcement %q", r.Id)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	log.Printf("%s edited announcement %s", c.Username(), a.ID)
	return toPB(a), nil
}

func (s *server) DeleteAnnouncement(ctx context.Context, r *pb.AnnouncementId) (*pb.Empty, error) {
	c, err := authz.Require(ctx, authz.RoleRegistrar)
	if err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM announcements WHERE id = $1`, r.Id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return status.Errorf(codes.NotFound, "no announcement %q", r.Id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("%s deleted announcement %s", c.Username(), r.Id)
	return &pb.Empty{}, nil
}

func Run(cfg *config.Config) error {
	d, err := db.Open(cfg, "announce", setup(cfg.Location))
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.AnnounceAddr))
	if err != nil {
		return err
	}
	// Same as course and grade: verify the JWT locally, ask the auth node
	// whether the session was logged out, and let the call through if auth
	// cannot be reached.
	revoker := authz.RemoteRevoker(cfg.AuthAddr, cfg.RevocationTimeoutDur, cfg.RevocationCacheTTL)
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret, revoker, db.Public...)))
	pb.RegisterAnnounceServer(g, &server{db: d})
	pb.RegisterHealthServer(g, db.Health(d))
	fmt.Println("announce node listening on", lis.Addr())
	return g.Serve(lis)
}
