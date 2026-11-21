// p4 runs one node of the enrollment system: p4 [-config file] <view|auth|course|grade|announce>
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	_ "time/tzdata" // the slim container image has no zoneinfo; embed it

	"p4/internal/announce"
	"p4/internal/auth"
	"p4/internal/config"
	"p4/internal/course"
	"p4/internal/grade"
	"p4/internal/view"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()
	nodes := map[string]func(*config.Config) error{
		"view": view.Run, "auth": auth.Run, "course": course.Run, "grade": grade.Run,
		"announce": announce.Run,
	}
	run, ok := nodes[flag.Arg(0)]
	if flag.NArg() != 1 || !ok {
		fmt.Fprintln(os.Stderr, "usage: p4 [-config file] <view|auth|course|grade|announce>")
		os.Exit(2)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(run(cfg))
}
