// Package course is the course node: list courses and enroll students.
package course

import (
	"context"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"p4/internal/authz"
	"p4/internal/config"
	"p4/internal/store"
	pb "p4/proto"
)

type Course struct {
	Code     string   `json:"code"`
	Title    string   `json:"title"`
	Faculty  string   `json:"faculty"`
	Capacity int      `json:"capacity"`
	Open     bool     `json:"open"`
	Students []string `json:"students"`
}

type data struct {
	Courses []Course `json:"courses"`
}

func seed() data {
	return data{Courses: []Course{
		{"CSOPESY", "Operating Systems", "Prof. Cruz", 3, true, nil},
		{"STDISCM", "Distributed Computing", "Prof. Cruz", 2, true, nil},
		{"CSARCH2", "Computer Architecture 2", "Prof. Lim", 3, true, nil},
		{"CCPROG3", "Object-Oriented Programming", "Prof. Lim", 3, false, nil},
	}}
}

type server struct {
	pb.UnimplementedCourseServer
	db *store.Store[data]
}

func (s *server) ListCourses(ctx context.Context, _ *pb.Empty) (*pb.CourseList, error) {
	c, err := authz.Require(ctx, "")
	if err != nil {
		return nil, err
	}
	out := &pb.CourseList{}
	s.db.Read(func(d *data) {
		for _, co := range d.Courses {
			out.Courses = append(out.Courses, &pb.CourseInfo{
				Code: co.Code, Title: co.Title, Faculty: co.Faculty,
				Capacity: int32(co.Capacity), Enrolled: int32(len(co.Students)), Open: co.Open,
				EnrolledByMe: slices.Contains(co.Students, c.Username()),
			})
		}
	})
	return out, nil
}

func (s *server) Enroll(ctx context.Context, r *pb.EnrollRequest) (*pb.Empty, error) {
	c, err := authz.Require(ctx, authz.RoleStudent)
	if err != nil {
		return nil, err
	}
	err = s.db.Update(func(d *data) error {
		i := slices.IndexFunc(d.Courses, func(co Course) bool { return co.Code == r.Code })
		if i < 0 {
			return status.Errorf(codes.NotFound, "no course %q", r.Code)
		}
		co := &d.Courses[i]
		switch {
		case !co.Open:
			return status.Errorf(codes.FailedPrecondition, "%s is not open for enrollment", co.Code)
		case slices.Contains(co.Students, c.Username()):
			return status.Errorf(codes.AlreadyExists, "already enrolled in %s", co.Code)
		case len(co.Students) >= co.Capacity:
			return status.Errorf(codes.ResourceExhausted, "%s is full", co.Code)
		}
		co.Students = append(co.Students, c.Username())
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("%s enrolled in %s", c.Username(), r.Code)
	return &pb.Empty{}, nil
}

func Run(cfg *config.Config) error {
	db, err := store.Open(filepath.Join(cfg.DataDir, "course.json"), seed)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", config.ListenAddr(cfg.CourseAddr))
	if err != nil {
		return err
	}
	g := grpc.NewServer(grpc.UnaryInterceptor(authz.Interceptor(cfg.JWTSecret)))
	pb.RegisterCourseServer(g, &server{db: db})
	fmt.Println("course node listening on", lis.Addr())
	return g.Serve(lis)
}
