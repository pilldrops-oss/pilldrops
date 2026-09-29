// Package functions is the serverless layer: named HTTP-handler functions with
// durable code, backed by sandboxes created on first invoke and reused while warm.
package functions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

var (
	// ErrExists is returned when registering a name that already exists in the org.
	ErrExists = errors.New("function already exists")
	// ErrNotFound is returned for an unknown function.
	ErrNotFound = errors.New("function not found")
	// ErrInvalidName is returned for a name that is not filesystem- and URL-safe.
	ErrInvalidName = errors.New("invalid function name")
)

var validNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validName(name string) bool { return validNameRe.MatchString(name) }

// FileSpec is one handler file to deploy into the sandbox.
type FileSpec struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// RegisterSpec is a function registration request.
type RegisterSpec struct {
	Name        string            `json:"name"`
	Template    string            `json:"template"`
	Start       string            `json:"start"`
	Port        int               `json:"port"`
	VCPU        int               `json:"vcpu"`         // 0 = template default
	MemoryMB    int               `json:"memory_mb"`    // 0 = template default
	MaxReplicas int               `json:"max_replicas"` // 0 = default; burst ceiling
	Env         map[string]string `json:"env"`
	Files       []FileSpec        `json:"files"`
}

// Function is the public view of a registered function.
type Function struct {
	Name          string            `json:"name"`
	Template      string            `json:"template"`
	Start         string            `json:"start"`
	Port          int               `json:"port"`
	VCPU          int               `json:"vcpu"`
	MemoryMB      int               `json:"memory_mb"`
	MaxReplicas   int               `json:"max_replicas"`
	Replicas      int               `json:"replicas"` // live count (runtime)
	SandboxID     string            `json:"sandbox_id"`
	Invokes       int64             `json:"invokes"`
	LastInvokedAt int64             `json:"last_invoked_at"`
	Env           map[string]string `json:"env"`
	ColdStarts    int64             `json:"cold_starts"`
	CreatedAt     int64             `json:"created_at"`
	UpdatedAt     int64             `json:"updated_at"`
}

// Registry orchestrates function metadata, durable code, and the sandbox lifecycle.
type Registry struct {
	d       *daemon.Daemon
	st      *store.Store
	codeDir string

	mu    sync.Mutex             // guards locks
	locks map[string]*sync.Mutex // per org/name

	poolMu sync.Mutex         // guards pools
	pools  map[string]*fnPool // per org/name

	stop     chan struct{}
	stopOnce sync.Once
}

// Close halts the background reaper. Idempotent.
func (r *Registry) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// NewRegistry builds a Registry storing durable code under codeDir.
func NewRegistry(d *daemon.Daemon, st *store.Store, codeDir string) *Registry {
	// Best-effort; a real failure resurfaces from writeCode at the first Register.
	_ = os.MkdirAll(codeDir, 0o755)
	r := &Registry{d: d, st: st, codeDir: codeDir,
		locks: map[string]*sync.Mutex{}, pools: map[string]*fnPool{},
		stop: make(chan struct{})}
	go r.reap()
	return r
}

func toFunction(r store.FunctionRecord) Function {
	env := map[string]string{}
	if r.Env != "" {
		_ = json.Unmarshal([]byte(r.Env), &env)
	}
	return Function{
		Name: r.Name, Template: r.Template, Start: r.StartCmd, Port: r.Port,
		VCPU: r.VCPU, MemoryMB: r.MemMiB, MaxReplicas: r.MaxReplicas,
		SandboxID: r.SandboxID, Invokes: r.Invokes, LastInvokedAt: r.LastInvokedAt,
		Env: env, ColdStarts: r.ColdStarts,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// marshalEnv returns the env map as JSON, "{}" when empty or on error.
func marshalEnv(env map[string]string) string {
	if len(env) == 0 {
		return "{}"
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (r *Registry) fnDir(org, name string) string {
	return filepath.Join(r.codeDir, org, name)
}

func (r *Registry) writeCode(org, name string, files []FileSpec) error {
	base := r.fnDir(org, name)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		dst := filepath.Join(base, filepath.Clean("/"+f.Path)) // clamp to base
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Register creates a new function without a sandbox. Returns ErrExists for a taken name.
func (r *Registry) Register(ctx context.Context, org string, spec RegisterSpec) (Function, error) {
	if !validName(spec.Name) {
		return Function{}, ErrInvalidName
	}
	if _, ok, err := r.st.GetFunction(ctx, org, spec.Name); err != nil {
		return Function{}, err
	} else if ok {
		return Function{}, ErrExists
	}
	if spec.Port == 0 {
		spec.Port = 8000
	}
	if err := r.writeCode(org, spec.Name, spec.Files); err != nil {
		return Function{}, err
	}
	now := time.Now().Unix()
	rec := store.FunctionRecord{
		Name: spec.Name, OrgID: org, Template: spec.Template, StartCmd: spec.Start,
		Port: spec.Port, VCPU: spec.VCPU, MemMiB: spec.MemoryMB, MaxReplicas: spec.MaxReplicas,
		Env: marshalEnv(spec.Env), CreatedAt: now, UpdatedAt: now,
	}
	if err := r.st.SaveFunction(ctx, rec); err != nil {
		return Function{}, err
	}
	return toFunction(rec), nil
}

// Get returns one function.
func (r *Registry) Get(ctx context.Context, org, name string) (Function, bool, error) {
	rec, ok, err := r.st.GetFunction(ctx, org, name)
	if err != nil || !ok {
		return Function{}, ok, err
	}
	return toFunction(rec), true, nil
}

// List returns all functions in an org.
func (r *Registry) List(ctx context.Context, org string) ([]Function, error) {
	recs, err := r.st.LoadFunctions(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Function, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toFunction(rec))
	}
	return out, nil
}

// Code returns the deployed handler files. Returns ErrNotFound for an unknown function.
func (r *Registry) Code(ctx context.Context, org, name string) ([]FileSpec, error) {
	if _, ok, err := r.st.GetFunction(ctx, org, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotFound
	}
	base := r.fnDir(org, name)
	out := []FileSpec{} // non-nil so an empty function serializes as [] not null
	err := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, FileSpec{Path: rel, Content: string(data)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete destroys the function's sandbox, its code, and its row.
func (r *Registry) Delete(ctx context.Context, org, name string) error {
	rec, ok, err := r.st.GetFunction(ctx, org, name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if rec.SandboxID != "" {
		_ = r.d.Destroy(context.Background(), rec.SandboxID)
	}
	_ = os.RemoveAll(r.fnDir(org, name))
	return r.st.DeleteFunction(ctx, org, name)
}
