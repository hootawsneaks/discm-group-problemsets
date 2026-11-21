// Package grade is the grade node: students view their grades, faculty upload
// them. Grades live in the grade node's own database.
package grade

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/db"
	pb "p4/proto"
)

func setup(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS grades (
			id      bigserial,
			student text NOT NULL,
			course  text NOT NULL,
			term    text NOT NULL,
			grade   text NOT NULL,
			faculty text NOT NULL, -- username of the uploader
			PRIMARY KEY (student, course, term)
		)`)
	if err != nil {
		return err
	}
	return db.SeedOnce(ctx, tx, func() error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO grades (student, course, term, grade, faculty) VALUES
				('alice', 'CCPROG1', 'T1 2024-2025', '4.0', 'prof2'),
				('alice', 'CCPROG2', 'T2 2024-2025', '3.5', 'prof2'),
				('bob',   'CCPROG1', 'T1 2024-2025', '3.0', 'prof2')
			ON CONFLICT DO NOTHING`)
		return err
	})
}

var validGrades = []string{"4.0", "3.5", "3.0", "2.5", "2.0", "1.5", "1.0", "0.0"}

type server struct {
	pb.UnimplementedGradeServer
	db *db.DB
}

// list returns the grades whose column (student or faculty) equals who.
func (s *server) list(ctx context.Context, column, who string) (*pb.GradeList, error) {
	out := &pb.GradeList{}
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT student, course, term, grade, faculty FROM grades WHERE `+column+` = $1 ORDER BY id`, who)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			g := &pb.GradeEntry{}
			if err := rows.Scan(&g.Student, &g.Course, &g.Term, &g.Grade, &g.Faculty); err != nil {
				return err
			}
			out.Grades = append(out.Grades, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) MyGrades(ctx context.Context, _ *pb.Empty) (*pb.GradeList, error) {
	c, err := authz.Require(ctx, authz.RoleStudent)
	if err != nil {
		return nil, err
	}
	return s.list(ctx, "student", c.Username())
}

func (s *server) UploadedGrades(ctx context.Context, _ *pb.Empty) (*pb.GradeList, error) {
	c, err := authz.Require(ctx, authz.RoleFaculty)
	if err != nil {
		return nil, err
	}
	return s.list(ctx, "faculty", c.Username())
}

// UploadGrade adds a grade, or replaces the existing one for the same student/course/term.
func (s *server) UploadGrade(ctx context.Context, r *pb.GradeEntry) (*pb.Empty, error) {
	c, err := authz.Require(ctx, authz.RoleFaculty)
	if err != nil {
		return nil, err
	}
	g := &pb.GradeEntry{
		Student: strings.TrimSpace(r.Student),
		Course:  strings.ToUpper(strings.TrimSpace(r.Course)),
		Term:    strings.TrimSpace(r.Term),
		Grade:   strings.TrimSpace(r.Grade),
		Faculty: c.Username(),
	}
	if g.Student == "" || g.Course == "" || g.Term == "" {
		return nil, status.Error(codes.InvalidArgument, "student, course and term are required")
	}
	if !slices.Contains(validGrades, g.Grade) {
		return nil, status.Errorf(codes.InvalidArgument, "grade must be one of %s", strings.Join(validGrades, ", "))
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO grades (student, course, term, grade, faculty) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (student, course, term) DO UPDATE SET grade = excluded.grade, faculty = excluded.faculty`,
			g.Student, g.Course, g.Term, g.Grade, g.Faculty)
		return err
	})
	if err != nil {
		return nil, err
	}
	log.Printf("%s uploaded %s for %s in %s (%s)", c.Username(), g.Grade, g.Student, g.Course, g.Term)
	return &pb.Empty{}, nil
}

func Run(cfg *config.Config) error {
	d, err := db.Open(cfg, "grade", setup)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.GradeAddr))
	if err != nil {
		return err
	}
	// Verify the JWT locally, and ask the auth node whether the session was
	// logged out. If auth is unreachable the call is allowed through.
	revoker := authz.RemoteRevoker(cfg.AuthAddr, cfg.RevocationTimeoutDur, cfg.RevocationCacheTTL)
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret, revoker, db.Public...)))
	pb.RegisterGradeServer(g, &server{db: d})
	pb.RegisterHealthServer(g, db.Health(d))
	fmt.Println("grade node listening on", lis.Addr())
	return g.Serve(lis)
}
