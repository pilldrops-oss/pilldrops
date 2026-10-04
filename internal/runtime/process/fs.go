package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// errTraversal is returned when a path would escape the sandbox root.
var errTraversal = fmt.Errorf("%w: path escapes sandbox root", rt.ErrPathRefused)

// workdirFS implements runtime.FileSystem against a single host directory,
// confining every path to root.
type workdirFS struct{ root string }

// resolve joins p to root; a path that would escape root is rejected, not redirected.
func (f *workdirFS) resolve(p string) (string, error) {
	full := filepath.Clean(filepath.Join(f.root, p))
	rel, err := filepath.Rel(f.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errTraversal
	}
	return full, nil
}

// hide rewrites a *PathError to the sandbox-relative path so the host location never leaks.
func (f *workdirFS) hide(err error) error {
	var pe *os.PathError
	if !errors.As(err, &pe) {
		return err
	}
	rel, rerr := filepath.Rel(f.root, pe.Path)
	if rerr != nil {
		rel = filepath.Base(pe.Path)
	}
	pe.Path = "/" + filepath.ToSlash(rel)
	// The errno already answers errors.Is for not-exist, exist and permission;
	// this adds the kinds it does not, such as ENOTDIR, the way every runtime says them.
	if kind := rt.FSKind(pe.Err.Error()); kind != nil {
		return fmt.Errorf("%w: %w", kind, pe)
	}
	return pe
}

func (f *workdirFS) Read(ctx context.Context, p string) ([]byte, error) {
	full, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	return data, f.hide(err)
}

func (f *workdirFS) Write(ctx context.Context, p string, data []byte) error {
	full, err := f.resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return f.hide(err)
	}
	return f.hide(os.WriteFile(full, data, 0o644))
}

func (f *workdirFS) List(ctx context.Context, p string) ([]rt.DirEntry, error) {
	full, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(full)
	if err != nil {
		return nil, f.hide(err)
	}
	out := make([]rt.DirEntry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, rt.DirEntry{
			Name: de.Name(), IsDir: de.IsDir(), Size: info.Size(), Modified: info.ModTime(),
		})
	}
	return out, nil
}

func (f *workdirFS) Mkdir(ctx context.Context, p string) error {
	full, err := f.resolve(p)
	if err != nil {
		return err
	}
	return f.hide(os.MkdirAll(full, 0o755))
}

func (f *workdirFS) Move(ctx context.Context, src, dst string) error {
	s, err := f.resolve(src)
	if err != nil {
		return err
	}
	d, err := f.resolve(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
		return f.hide(err)
	}
	return f.hide(os.Rename(s, d))
}

func (f *workdirFS) Remove(ctx context.Context, p string) error {
	full, err := f.resolve(p)
	if err != nil {
		return err
	}
	// "", "." and "/" all resolve to root, and RemoveAll would wipe the sandbox.
	if full == filepath.Clean(f.root) {
		return fmt.Errorf("%w: refusing to remove sandbox root", os.ErrInvalid)
	}
	return f.hide(os.RemoveAll(full))
}

func (f *workdirFS) Stat(ctx context.Context, p string) (rt.FileInfo, error) {
	full, err := f.resolve(p)
	if err != nil {
		return rt.FileInfo{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return rt.FileInfo{}, f.hide(err)
	}
	return rt.FileInfo{
		Name: info.Name(), IsDir: info.IsDir(), Size: info.Size(),
		Mode: info.Mode().String(), Modified: info.ModTime(),
	}, nil
}
