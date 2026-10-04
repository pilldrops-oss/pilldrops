package templates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var reg struct {
	mu       sync.Mutex
	dir      string
	scanned  []Template
	lastScan time.Time
}

// scanTTL bounds how often the image directory is re-read.
const scanTTL = 5 * time.Second

// SetDir points the registry at the directory holding <id>/rootfs.squashfs and
// optional <id>/template.json. Empty disables disk discovery.
func SetDir(dir string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.dir = dir
	reg.lastScan = time.Time{}
}

// Dir returns the configured image directory, empty when disk discovery is off.
func Dir() string {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.dir
}

// SizeMB returns the on-disk size of a template's rootfs.squashfs in megabytes, 0 if absent.
func SizeMB(id string) int {
	dir := Dir()
	if dir == "" {
		return 0
	}
	fi, err := os.Stat(filepath.Join(dir, id, "rootfs.squashfs"))
	if err != nil {
		return 0
	}
	return int(fi.Size() / (1024 * 1024))
}

// PublishedIDs returns the ids holding a rootfs.squashfs, which is exactly
// what the Firecracker vm layer can boot. Nil when disk discovery is off.
func PublishedIDs() []string {
	dir := Dir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "rootfs.squashfs")); err == nil {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// discovered returns the disk templates, rescanning at most every scanTTL.
func discovered() []Template {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.dir == "" {
		return nil
	}
	if time.Since(reg.lastScan) < scanTTL {
		return reg.scanned
	}
	reg.lastScan = time.Now()
	entries, err := os.ReadDir(reg.dir)
	if err != nil {
		reg.scanned = nil
		return nil
	}
	var out []Template
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if _, err := os.Stat(filepath.Join(reg.dir, id, "rootfs.squashfs")); err != nil {
			continue
		}
		out = append(out, loadManifest(reg.dir, id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	reg.scanned = out
	return out
}

// loadManifest builds the Template for one on-disk image: manifest fields win,
// then the compiled-in entry with the same id, then generic defaults.
func loadManifest(dir, id string) Template {
	t, _ := builtin(id)
	t.ID = id
	if t.Name == "" {
		t.Name = id
	}
	if raw, err := os.ReadFile(filepath.Join(dir, id, "template.json")); err == nil {
		var m Template
		if json.Unmarshal(raw, &m) == nil {
			// Field by field, and deliberately not json.Unmarshal(raw, &t): a
			// field the manifest leaves out decodes as the zero value and would
			// erase the compiled-in entry underneath, and Image/Ports are
			// platform-defined (they skip the node's image allowlist), so a
			// manifest must not be able to set them at all.
			override(&t.Name, m.Name)
			override(&t.Description, m.Description)
			override(&t.Language, m.Language)
			override(&t.Icon, m.Icon)
			override(&t.Visibility, m.Visibility)
			override(&t.StartSuggestion, m.StartSuggestion)
			override(&t.ServerDesc, m.ServerDesc)
			override(&t.DocsURL, m.DocsURL)
			override(&t.Kind, m.Kind)
			override(&t.Outcome, m.Outcome)
			override(&t.HasServer, m.HasServer)
			override(&t.PrimaryInput, m.PrimaryInput)
			overridePositive(&t.CPU, m.CPU)
			overridePositive(&t.MemoryMB, m.MemoryMB)
			overridePositive(&t.StorageMB, m.StorageMB)
			overridePositive(&t.Port, m.Port)
			overridePositive(&t.DefaultPort, m.DefaultPort)
			overrideSlice(&t.Tags, m.Tags)
			overrideSlice(&t.Preinstalled, m.Preinstalled)
			overrideSlice(&t.SetupSuggestion, m.SetupSuggestion)
			overrideSlice(&t.Env, m.Env)
		}
	}
	if t.CPU <= 0 {
		t.CPU = 1
	}
	if t.MemoryMB <= 0 {
		t.MemoryMB = 512
	}
	if t.StorageMB <= 0 {
		t.StorageMB = 1024
	}
	if t.Visibility == "" {
		t.Visibility = "public"
	}
	if t.DefaultPort <= 0 {
		if t.Port > 0 {
			t.DefaultPort = t.Port
		} else {
			t.DefaultPort = 8000
		}
	}
	return t
}

// override sets *dst to v unless v is the zero value, which is what a field the
// manifest left out decodes as.
func override[T comparable](dst *T, v T) {
	var zero T
	if v != zero {
		*dst = v
	}
}

// overridePositive is override for a count or a port, where zero and negative
// are both "not stated".
func overridePositive(dst *int, v int) {
	if v > 0 {
		*dst = v
	}
}

// overrideSlice is override for slices, which are not comparable.
func overrideSlice[T any](dst *[]T, v []T) {
	if len(v) > 0 {
		*dst = v
	}
}

// builtin looks up the compiled-in catalog by canonical ID or alias.
func builtin(id string) (Template, bool) {
	for _, t := range catalog {
		if t.ID == id {
			return t, true
		}
		for _, a := range t.Aliases {
			if a == id {
				return t, true
			}
		}
	}
	return Template{}, false
}
