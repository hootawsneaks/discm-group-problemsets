// Package store is a tiny JSON-file store. Each node owns its own file, so
// no node depends on another node's data.
package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

type Store[T any] struct {
	mu   sync.Mutex
	path string
	data T
}

// Open loads path, or initialises it with seed() if it doesn't exist yet.
func Open[T any](path string, seed func() T) (*Store[T], error) {
	s := &Store[T]{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.data = seed()
		return s, s.save()
	case err != nil:
		return nil, err
	}
	return s, json.Unmarshal(b, &s.data)
}

// Read runs f with the data locked.
func (s *Store[T]) Read(f func(*T)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.data)
}

// Update runs f with the data locked and saves it if f returns nil.
func (s *Store[T]) Update(f func(*T) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := f(&s.data); err != nil {
		return err
	}
	return s.save()
}

func (s *Store[T]) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
