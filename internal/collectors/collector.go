package collectors

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"tokenanalyse/internal/model"
)

type Context struct {
	DeviceID string
	Home     string
	Env      map[string]string
	Paths    Paths
}

type Paths struct {
	ClaudeCode  string
	Codex       string
	Gemini      string
	Grok        string
	OpenCode    string
	Pi          string
	OMP         string
	MiniMax     string
	Kimi        string
	Copilot     string
	CodeBuddy   string
	WorkBuddy   string
	DSH         string
	MCode       string
	Antigravity string
}

func DetectContext(deviceID string) Context {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	env := map[string]string{}
	for _, entry := range os.Environ() {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				env[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	paths := Paths{
		ClaudeCode:  envOr(env, "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")),
		Codex:       envOr(env, "CODEX_HOME", filepath.Join(home, ".codex")),
		Gemini:      envOr(env, "GEMINI_HOME", filepath.Join(home, ".gemini")),
		Grok:        envOr(env, "GROK_HOME", filepath.Join(home, ".grok")),
		OpenCode:    envOr(env, "OPENCODE_DATA_DIR", filepath.Join(home, ".local", "share", "opencode")),
		Pi:          envOr(env, "PI_AGENT_DIR", filepath.Join(home, ".pi")),
		OMP:         envOr(env, "OMP_HOME", filepath.Join(home, ".omp")),
		MiniMax:     envOr(env, "MINIMAX_HOME", filepath.Join(home, ".minimax")),
		Kimi:        envOr(env, "KIMI_HOME", filepath.Join(home, ".kimi")),
		Copilot:     envOr(env, "COPILOT_HOME", filepath.Join(home, ".copilot")),
		CodeBuddy:   envOr(env, "CODEBUDDY_HOME", filepath.Join(home, ".codebuddy")),
		WorkBuddy:   envOr(env, "WORKBUDDY_HOME", filepath.Join(home, ".workbuddy")),
		DSH:         envOr(env, "DSH_HOME", filepath.Join(home, ".deepseek")),
		MCode:       envOr(env, "MCODE_HOME", filepath.Join(home, ".mcode")),
		Antigravity: envOr(env, "AGY_HOME", filepath.Join(home, ".agy")),
	}
	return Context{DeviceID: deviceID, Home: home, Env: env, Paths: paths}
}

func envOr(env map[string]string, key, fallback string) string {
	if value := env[key]; value != "" {
		return value
	}
	return fallback
}

type Cursor struct {
	SourceID      string    `json:"sourceId"`
	FilePath      string    `json:"filePath"`
	Offset        int64     `json:"offset"`
	ModifiedNanos int64     `json:"modifiedNanos"`
	State         string    `json:"state,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type Result struct {
	Events       []model.UsageEvent `json:"events"`
	Cursors      []Cursor           `json:"cursors"`
	FilesScanned int                `json:"filesScanned"`
	BytesRead    int64              `json:"bytesRead"`
	Errors       []string           `json:"errors"`
	Metadata     map[string]any     `json:"metadata,omitempty"`
}

func (r *Result) Merge(other Result) {
	r.Events = append(r.Events, other.Events...)
	r.Cursors = append(r.Cursors, other.Cursors...)
	r.FilesScanned += other.FilesScanned
	r.BytesRead += other.BytesRead
	r.Errors = append(r.Errors, other.Errors...)
	if other.Metadata != nil {
		if r.Metadata == nil {
			r.Metadata = map[string]any{}
		}
		for key, value := range other.Metadata {
			r.Metadata[key] = value
		}
	}
}

type Info struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AppTypes    []string `json:"appTypes"`
	Description string   `json:"description"`
	Incremental bool     `json:"incremental"`
}

type Collector interface {
	Info() Info
	Collect(ctx Context, cursors []Cursor) (Result, error)
}

type Registry struct {
	mu         sync.RWMutex
	collectors []Collector
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) Register(collector Collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, collector)
}

func (r *Registry) Infos() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	infos := make([]Info, 0, len(r.collectors))
	for _, collector := range r.collectors {
		infos = append(infos, collector.Info())
	}
	return infos
}

func (r *Registry) CollectAll(ctx Context, cursors []Cursor) Result {
	r.mu.RLock()
	collectors := append([]Collector(nil), r.collectors...)
	r.mu.RUnlock()
	result := Result{Metadata: map[string]any{}}
	for _, collector := range collectors {
		partial, err := collector.Collect(ctx, cursors)
		if err != nil {
			result.Errors = append(result.Errors, collector.Info().ID+": "+err.Error())
			continue
		}
		result.Merge(partial)
	}
	return result
}

func (r *Registry) CollectOne(id string, ctx Context, cursors []Cursor) (Result, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, collector := range r.collectors {
		if collector.Info().ID == id {
			return collector.Collect(ctx, cursors)
		}
	}
	return Result{}, os.ErrNotExist
}
