// Package course is the course node: list courses, enroll and drop. Courses
// and enrollments live in the course node's own database.
package course

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"

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
		CREATE TABLE IF NOT EXISTS courses (
			code     text PRIMARY KEY,
			title    text NOT NULL,
			faculty  text NOT NULL,
			capacity int  NOT NULL CHECK (capacity >= 0),
			open     boolean NOT NULL
		);
		CREATE TABLE IF NOT EXISTS enrollments (
			course      text NOT NULL REFERENCES courses (code),
			student     text NOT NULL,
			enrolled_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (course, student)
		)`)
	if err != nil {
		return err
	}
	return db.SeedOnce(ctx, tx, func() error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO courses VALUES
				('CSOPESY', 'Operating Systems',           'Prof. Cruz', 3, true),
				('STDISCM', 'Distributed Computing',       'Prof. Cruz', 2, true),
				('CSARCH2', 'Computer Architecture 2',     'Prof. Lim',  3, true),
				('CCPROG3', 'Object-Oriented Programming', 'Prof. Lim',  3, false)
			ON CONFLICT DO NOTHING`)
		return err
	})
}

type server struct {
	pb.UnimplementedCourseServer
	db *db.DB
}

func (s *server) ListCourses(ctx context.Context, _ *pb.Empty) (*pb.CourseList, error) {
	c, err := authz.Require(ctx, "")
	if err != nil {
		return nil, err
	}
	out := &pb.CourseList{}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT c.code, c.title, c.faculty, c.capacity, c.open,
			       (SELECT count(*) FROM enrollments e WHERE e.course = c.code),
			       EXISTS (SELECT 1 FROM enrollments e WHERE e.course = c.code AND e.student = $1)
			FROM courses c ORDER BY c.code`, c.Username())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ci := &pb.CourseInfo{}
			if err := rows.Scan(&ci.Code, &ci.Title, &ci.Faculty, &ci.Capacity, &ci.Open, &ci.Enrolled, &ci.EnrolledByMe); err != nil {
				return err
			}
			out.Courses = append(out.Courses, ci)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) Enroll(ctx context.Context, r *pb.EnrollRequest) (*pb.Empty, error) {
	c, err := authz.Require(ctx, authz.RoleStudent)
	if err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		// Lock the course row so two students can't both take the last slot.
		var open bool
		var capacity, enrolled int
		err := tx.QueryRowContext(ctx, `SELECT open, capacity FROM courses WHERE code = $1 FOR UPDATE`, r.Code).
			Scan(&open, &capacity)
		if errors.Is(err, sql.ErrNoRows) {
			return status.Errorf(codes.NotFound, "no course %q", r.Code)
		}
		if err != nil {
			return err
		}
		var mine bool
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*), coalesce(bool_or(student = $2), false) FROM enrollments WHERE course = $1`,
			r.Code, c.Username()).Scan(&enrolled, &mine); err != nil {
			return err
		}
		switch {
		case !open:
			return status.Errorf(codes.FailedPrecondition, "%s is not open for enrollment", r.Code)
		case mine:
			return status.Errorf(codes.AlreadyExists, "already enrolled in %s", r.Code)
		case enrolled >= capacity:
			return status.Errorf(codes.ResourceExhausted, "%s is full", r.Code)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO enrollments (course, student) VALUES ($1, $2)`, r.Code, c.Username())
		return err
	})
	if err != nil {
		return nil, err
	}
	log.Printf("%s enrolled in %s", c.Username(), r.Code)
	return &pb.Empty{}, nil
}

// Drop removes the student from a course they enrolled in.
func (s *server) Drop(ctx context.Context, r *pb.EnrollRequest) (*pb.Empty, error) {
	c, err := authz.Require(ctx, authz.RoleStudent)
	if err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM enrollments WHERE course = $1 AND student = $2`, r.Code, c.Username())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM courses WHERE code = $1)`, r.Code).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return status.Errorf(codes.NotFound, "no course %q", r.Code)
		}
		return status.Errorf(codes.FailedPrecondition, "not enrolled in %s", r.Code)
	})
	if err != nil {
		return nil, err
	}
	log.Printf("%s dropped %s", c.Username(), r.Code)
	return &pb.Empty{}, nil
}

func Run(cfg *config.Config) error {
	d, err := db.Open(cfg, "course", setup)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.CourseAddr))
	if err != nil {
		return err
	}
	// Verify the JWT locally, and ask the auth node whether the session was
	// logged out. If auth is unreachable the call is allowed through.
	revoker := authz.RemoteRevoker(cfg.AuthAddr, cfg.RevocationTimeoutDur, cfg.RevocationCacheTTL)
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret, revoker, db.Public...)))
	pb.RegisterCourseServer(g, &server{db: d})
	pb.RegisterHealthServer(g, db.Health(d))
	fmt.Println("course node listening on", lis.Addr())
	return g.Serve(lis)
}
