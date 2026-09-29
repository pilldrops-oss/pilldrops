package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// lockFor returns the per-function mutex, creating it on first use.
func (r *Registry) lockFor(org, name string) *sync.Mutex {
	key := org + "/" + name
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.locks[key]
	if !ok {
		m = &sync.Mutex{}
		r.locks[key] = m
	}
	return m
}

// EnsureSandbox returns the function's sandbox id, creating one if needed, and
// whether this counts as a cold start. It reports a paused sandbox as cold but
// does not wake it; PreviewTarget does that.
func (r *Registry) EnsureSandbox(ctx context.Context, org, name string) (string, bool, error) {
	lock := r.lockFor(org, name)
	lock.Lock()
	defer lock.Unlock()

	rec, ok, err := r.st.GetFunction(ctx, org, name)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, ErrNotFound
	}

	if rec.SandboxID != "" {
		if info, live := r.d.Get(rec.SandboxID); live {
			cold := info.State == daemon.StatePaused
			return rec.SandboxID, cold, nil
		}
	}

	id, err := r.deploy(ctx, rec)
	if err != nil {
		return "", false, err
	}
	rec.SandboxID = id
	rec.UpdatedAt = time.Now().Unix()
	if err := r.st.SaveFunction(ctx, rec); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// deploy creates a sandbox, writes the function's code into it, and starts the handler.
func (r *Registry) deploy(ctx context.Context, rec store.FunctionRecord) (string, error) {
	sb, err := r.d.Create(ctx, daemon.CreateSpec{
		Template: rec.Template, OrgID: rec.OrgID,
		VCPU: rec.VCPU, MemMiB: rec.MemMiB,
	})
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	fs, err := r.d.FS(sb.ID)
	if err != nil {
		_ = r.d.Destroy(ctx, sb.ID)
		return "", fmt.Errorf("fs: %w", err)
	}
	base := r.fnDir(rec.OrgID, rec.Name)
	walkErr := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return fs.Write(ctx, rel, data)
	})
	if walkErr != nil {
		_ = r.d.Destroy(ctx, sb.ID)
		return "", fmt.Errorf("deploy code: %w", walkErr)
	}
	if rec.StartCmd != "" {
		env := map[string]string{}
		if rec.Env != "" {
			_ = json.Unmarshal([]byte(rec.Env), &env)
		}
		if err := r.d.StartServer(ctx, sb.ID, rec.StartCmd, rec.Port, env); err != nil {
			_ = r.d.Destroy(ctx, sb.ID)
			return "", fmt.Errorf("start: %w", err)
		}
	}
	return sb.ID, nil
}

// Update rewrites a function's code and config and destroys its sandbox so the
// next invoke redeploys.
func (r *Registry) Update(ctx context.Context, org, name string, spec RegisterSpec) error {
	lock := r.lockFor(org, name)
	lock.Lock()
	defer lock.Unlock()

	rec, ok, err := r.st.GetFunction(ctx, org, name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if spec.Port == 0 {
		spec.Port = 8000
	}
	// No files means a config-only update; keep the deployed handler.
	if len(spec.Files) > 0 {
		if err := r.writeCode(org, name, spec.Files); err != nil {
			return err
		}
	}
	if rec.SandboxID != "" {
		_ = r.d.Destroy(context.Background(), rec.SandboxID)
	}
	rec.Template = spec.Template
	rec.StartCmd = spec.Start
	rec.Port = spec.Port
	rec.VCPU = spec.VCPU
	rec.MemMiB = spec.MemoryMB
	rec.Env = marshalEnv(spec.Env)
	rec.SandboxID = ""
	rec.UpdatedAt = time.Now().Unix()
	return r.st.SaveFunction(ctx, rec)
}

// RecordInvoke bumps the invoke counters and last-invoked timestamp, best-effort.
func (r *Registry) RecordInvoke(ctx context.Context, org, name string, cold bool) {
	rec, ok, err := r.st.GetFunction(ctx, org, name)
	if err != nil || !ok {
		return
	}
	rec.Invokes++
	if cold {
		rec.ColdStarts++
	}
	rec.LastInvokedAt = time.Now().Unix()
	_ = r.st.SaveFunction(ctx, rec)
}

// PreviewTarget resolves the dial target for a sandbox, waking it if paused.
// org is checked by the daemon because nothing ties sandboxID to the caller's function.
func (r *Registry) PreviewTarget(ctx context.Context, sandboxID string, port int, org string) (string, error) {
	return r.d.PreviewTarget(ctx, sandboxID, port, org)
}
