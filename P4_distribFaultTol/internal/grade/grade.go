// Package grade is the grade node: students view their grades, faculty upload them.
package grade

import (
	"context"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/store"
	pb "p4/proto"
)

type Grade struct {
	Student string `json:"student"`
	Course  string `json:"course"`
	Term    string `json:"term"`
	Grade   string `json:"grade"`
	Faculty string `json:"faculty"` // username of the uploader
}

type data struct {
	Grades []Grade `json:"grades"`
}

func seed() data {
	return data{Grades: []Grade{
		{"alice", "CCPROG1", "T1 2024-2025", "4.0", "prof2"},
		{"alice", "CCPROG2", "T2 2024-2025", "3.5", "prof2"},
		{"bob", "CCPROG1", "T1 2024-2025", "3.0", "prof2"},
	}}
}

var validGrades = []string{"4.0", "3.5", "3.0", "2.5", "2.0", "1.5", "1.0", "0.0"}

type server struct {
	pb.UnimplementedGradeServer
	db *store.Store[data]
}

func (s *server) list(keep func(Grade) bool) *pb.GradeList {
	out := &pb.GradeList{}
	s.db.Read(func(d *data) {
		for _, g := range d.Grades {
			if keep(g) {
				out.Grades = append(out.Grades, &pb.GradeEntry{
					Student: g.Student, Course: g.Course, Term: g.Term, Grade: g.Grade, Faculty: g.Faculty,
				})
			}
		}
	})
	return out
}

func (s *server) MyGrades(ctx context.Context, _ *pb.Empty) (*pb.GradeList, error) {
	c, err := authz.Require(ctx, authz.RoleStudent)
	if err != nil {
		return nil, err
	}
	return s.list(func(g Grade) bool { return g.Student == c.Username() }), nil
}

func (s *server) UploadedGrades(ctx context.Context, _ *pb.Empty) (*pb.GradeList, error) {
	c, err := authz.Require(ctx, authz.RoleFaculty)
	if err != nil {
		return nil, err
	}
	return s.list(func(g Grade) bool { return g.Faculty == c.Username() }), nil
}

// UploadGrade adds a grade, or replaces the existing one for the same student/course/term.
func (s *server) UploadGrade(ctx context.Context, r *pb.GradeEntry) (*pb.Empty, error) {
	c, err := authz.Require(ctx, authz.RoleFaculty)
	if err != nil {
		return nil, err
	}
	g := Grade{
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
	err = s.db.Update(func(d *data) error {
		i := slices.IndexFunc(d.Grades, func(x Grade) bool {
			return x.Student == g.Student && x.Course == g.Course && x.Term == g.Term
		})
		if i >= 0 {
			d.Grades[i] = g
		} else {
			d.Grades = append(d.Grades, g)
		}
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	log.Printf("%s uploaded %s for %s in %s (%s)", c.Username(), g.Grade, g.Student, g.Course, g.Term)
	return &pb.Empty{}, nil
}

func Run(cfg *config.Config) error {
	db, err := store.Open(filepath.Join(cfg.DataDir, "grade.json"), seed)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.GradeAddr))
	if err != nil {
		return err
	}
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret)))
	pb.RegisterGradeServer(g, &server{db: db})
	fmt.Println("grade node listening on", lis.Addr())
	return g.Serve(lis)
}
