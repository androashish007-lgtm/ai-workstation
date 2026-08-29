// Package project implements lightweight chat grouping: a Project is just a
// name plus a shared notes/instructions blurb that gets folded into every
// chat inside it as extra background context. Chats keep fully independent
// message histories — a project only ever adds shared context, it never
// merges conversations together.
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Notes     string    `json:"notes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	projects map[string]*Project
}

func New(dataDir string) *Store {
	s := &Store{path: filepath.Join(dataDir, "projects.json"), projects: map[string]*Project{}}
	s.load()
	return s
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []Project
	if err := json.Unmarshal(b, &list); err != nil {
		return
	}
	for i := range list {
		p := list[i]
		s.projects[p.ID] = &p
	}
}

func (s *Store) save() {
	list := make([]Project, 0, len(s.projects))
	for _, p := range s.projects {
		list = append(list, *p)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.path)
	}
}

func (s *Store) Create(name, notes string) Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	p := Project{ID: fmt.Sprintf("%d", now.UnixNano()), Name: name, Notes: notes, CreatedAt: now, UpdatedAt: now}
	s.projects[p.ID] = &p
	s.save()
	return p
}

func (s *Store) Update(id, name, notes string) (Project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return Project{}, false
	}
	p.Name = name
	p.Notes = notes
	p.UpdatedAt = time.Now()
	s.save()
	return *p, true
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[id]; !ok {
		return false
	}
	delete(s.projects, id)
	s.save()
	return true
}

func (s *Store) Get(id string) (Project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return Project{}, false
	}
	return *p, true
}

func (s *Store) List() []Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
