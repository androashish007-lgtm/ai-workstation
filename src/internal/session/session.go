// Package session persists chat/generation history to data/sessions/*.json
// and tracks small bits of app state (last active session, last server
// settings) in data/config.json so a relaunch resumes where the user left
// off without asking.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn. ImagePath is set (and Content may be empty or hold
// the caption/prompt) when this turn produced a generated image.
// ImagePaths holds one or more images the user attached to their own
// message (as input for the assistant to look at, not something it made).
type Message struct {
	Role       Role     `json:"role"`
	Content    string   `json:"content"`
	ImagePath  string   `json:"image_path,omitempty"`
	ImagePaths []string `json:"image_paths,omitempty"`
	// Notice marks a persisted assistant message that stands in for a live
	// event the UI might have missed (no matching model installed, an
	// engine needs approval) — its value tells the frontend which live
	// suggestion/approval card to (re-)fetch and show alongside this
	// message, since the actual action (a Download or Approve button)
	// needs current data, not whatever was true when this was saved.
	Notice    string    `json:"notice,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Messages  []Message `json:"messages"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastTextID/LastImageID hold the user's explicit model pick for this
	// chat, from the model dropdown next to the composer — empty means
	// "Auto", i.e. leave it to the router's automatic hardware/complexity
	// fit as before. Set via PATCH /api/sessions/{id}, read by the router's
	// SelectTextModel/SelectImageModel preferredID param each turn.
	LastTextID  string `json:"last_text_model_id,omitempty"`
	LastImageID string `json:"last_image_model_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
}

type Summary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
	ProjectID string    `json:"project_id,omitempty"`
}

type Manager struct {
	mu  sync.Mutex
	dir string
}

func NewManager(dir string) *Manager {
	os.MkdirAll(dir, 0o755)
	return &Manager{dir: dir}
}

func (m *Manager) path(id string) string {
	return filepath.Join(m.dir, id+".json")
}

func (m *Manager) New(projectID string) *Session {
	now := time.Now()
	id := fmt.Sprintf("%d", now.UnixNano())
	return &Session{ID: id, Title: "New chat", CreatedAt: now, UpdatedAt: now, ProjectID: projectID}
}

func (m *Manager) Load(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := os.ReadFile(m.path(id))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Manager) Save(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path(s.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path(s.ID))
}

func (m *Manager) List() ([]Summary, error) {
	m.mu.Lock()
	entries, err := os.ReadDir(m.dir)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		s, err := m.Load(id)
		if err != nil {
			continue
		}
		out = append(out, Summary{ID: s.ID, Title: s.Title, UpdatedAt: s.UpdatedAt, ProjectID: s.ProjectID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// AutoTitle sets a session's title from its first user message the first
// time one arrives (simple truncation for Phase 1; background LLM-generated
// titles/tags/summaries are a Phase 2 enhancement).
func (s *Session) AutoTitle() {
	if s.Title != "New chat" && s.Title != "" {
		return
	}
	for _, msg := range s.Messages {
		if msg.Role == RoleUser && strings.TrimSpace(msg.Content) != "" {
			t := strings.TrimSpace(msg.Content)
			if len(t) > 60 {
				t = t[:60] + "…"
			}
			s.Title = t
			return
		}
	}
}

// Config is small persisted app state, separate from any one session, so
// relaunch can resume the right session/model without user input.
type Config struct {
	ActiveSessionID string `json:"active_session_id,omitempty"`
	Port            int    `json:"port,omitempty"`
	LANEnabled      bool   `json:"lan_enabled"`
	// ExtraModelDirsText/Image are additional folders (beyond this app's
	// own default models/text or models/image) also scanned for installed
	// models — e.g. a folder on a faster internal drive. The default
	// folder is always scanned too and isn't stored here.
	ExtraModelDirsText  []string `json:"extra_model_dirs_text,omitempty"`
	ExtraModelDirsImage []string `json:"extra_model_dirs_image,omitempty"`
}

type ConfigStore struct {
	path string
	mu   sync.Mutex
}

func NewConfigStore(dataDir string) *ConfigStore {
	return &ConfigStore{path: filepath.Join(dataDir, "config.json")}
}

func (c *ConfigStore) Load() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cfg Config
	b, err := os.ReadFile(c.path)
	if err != nil {
		return Config{LANEnabled: true}
	}
	json.Unmarshal(b, &cfg)
	return cfg
}

func (c *ConfigStore) Save(cfg Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
