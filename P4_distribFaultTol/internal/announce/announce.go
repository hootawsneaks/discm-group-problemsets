// Package announce is the announce node: registrar announcements such as the
// enrollment schedule per year level, enrollment confirmation and payment
// deadlines. It is its own object with its own full CRUD on its own node, and
// it needs no other node to answer: a student's year level comes from their
// token, so even with the auth node down, logged-in users can read it.
package announce

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/store"
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

type data struct {
	Announcements []Announcement `json:"announcements"`
}

func seed(loc *time.Location) func() data {
	return func() data {
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
		return data{Announcements: []Announcement{
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
		}}
	}
}

type server struct {
	pb.UnimplementedAnnounceServer
	db *store.Store[data]
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
	s.db.Read(func(d *data) {
		for _, a := range d.Announcements {
			if visible(c, a) {
				list = append(list, a)
			}
		}
	})
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
	var ok bool
	s.db.Read(func(d *data) {
		i := slices.IndexFunc(d.Announcements, func(x Announcement) bool { return x.ID == r.Id })
		if ok = i >= 0; ok {
			a = d.Announcements[i]
		}
	})
	// An announcement for another year level is reported as missing, so a
	// student cannot tell it exists.
	if !ok || !visible(c, a) {
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
	if err := s.db.Update(func(d *data) error {
		d.Announcements = append(d.Announcements, a)
		return nil
	}); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
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
	err = s.db.Update(func(d *data) error {
		i := slices.IndexFunc(d.Announcements, func(x Announcement) bool { return x.ID == r.Id })
		if i < 0 {
			return status.Errorf(codes.NotFound, "no announcement %q", r.Id)
		}
		old := d.Announcements[i]
		// The registrar is an office: any registrar account may edit any
		// announcement. Who first posted it and when are kept.
		a.ID, a.Author, a.AuthorName, a.CreatedAt = old.ID, old.Author, old.AuthorName, old.CreatedAt
		a.UpdatedAt = time.Now()
		d.Announcements[i] = a
		return nil
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
	err = s.db.Update(func(d *data) error {
		i := slices.IndexFunc(d.Announcements, func(x Announcement) bool { return x.ID == r.Id })
		if i < 0 {
			return status.Errorf(codes.NotFound, "no announcement %q", r.Id)
		}
		d.Announcements = slices.Delete(d.Announcements, i, i+1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("%s deleted announcement %s", c.Username(), r.Id)
	return &pb.Empty{}, nil
}

func Run(cfg *config.Config) error {
	db, err := store.Open(filepath.Join(cfg.DataDir, "announce.json"), seed(cfg.Location))
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
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret, revoker)))
	pb.RegisterAnnounceServer(g, &server{db: db})
	fmt.Println("announce node listening on", lis.Addr())
	return g.Serve(lis)
}
