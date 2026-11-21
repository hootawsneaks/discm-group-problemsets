// Package valid holds the length limits every node applies to text a client
// sends, so the limits and their wording stay the same across nodes.
//
// Without these, a single request can store a megabyte in a text column and
// every page that renders it afterwards has to carry it. The database columns
// are `text`, so nothing below the application bounds them either.
//
// The limits are generous — far above anything a real title, name or course
// code needs — because their job is to stop abuse and typos, not to second-
// guess the user.
package valid

import (
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Field limits, in characters. Named so a reader can see what each one is for.
const (
	// Username: an account name. The seeded ones are 5-9 characters.
	MaxUsername = 64
	// Password: bcrypt only reads the first 72 bytes anyway, so anything past
	// this is already ignored; the limit just stops a client sending megabytes
	// for us to hash.
	MaxPassword = 128
	// CourseCodeLen is exact, not a maximum: every course code here is seven
	// characters, e.g. "CSOPESY", "STDISCM", "CCPROG1". Anything else is not a
	// course code, so it is refused before a lookup rather than coming back as
	// "no such course".
	CourseCodeLen = 7
	// Term, e.g. "T1 2026-2027" (12). Room for a spelled-out term.
	MaxTerm = 64
	// Announcement title: a headline, shown in a list and as a page heading.
	MaxTitle = 200
	// Announcement body: a few paragraphs, roughly 800 words.
	MaxBody = 5000
	// An id we generated ourselves (16 hex characters) or a seed id. Bounded so
	// a nonsense lookup is refused before it reaches the database.
	MaxID = 64
)

// Text trims s and checks it is not empty and not longer than max characters.
// field names the input in the error, so the message tells the user which box
// to fix.
func Text(field, s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return s, status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	return s, TooLong(field, s, max)
}

// Optional is Text for a field that may be left empty.
func Optional(field, s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	return s, TooLong(field, s, max)
}

// Exactly trims s and checks it is exactly n characters long, for an input
// whose length is fixed by the thing it names rather than merely bounded.
func Exactly(field, s string, n int) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return s, status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	if got := utf8.RuneCountInString(s); got != n {
		return s, status.Errorf(codes.InvalidArgument,
			"%s must be exactly %d characters, this one is %d", field, n, got)
	}
	return s, nil
}

// TooLong reports an error if s is longer than max characters. Counting runes
// rather than bytes means a title in Filipino or with an emoji gets the same
// allowance as one in plain ASCII.
func TooLong(field, s string, max int) error {
	if n := utf8.RuneCountInString(s); n > max {
		return status.Errorf(codes.InvalidArgument,
			"%s is too long: %d characters, the most is %d", field, n, max)
	}
	return nil
}
