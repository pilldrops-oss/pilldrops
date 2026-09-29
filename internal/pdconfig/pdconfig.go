// Package pdconfig parses pilldrops.json, the committed file that declares a
// repository's sandbox configuration. Keep in step with web/src/lib/pilldropsConfig.ts.
package pdconfig

import (
	"encoding/json"
	"fmt"
)

// File is the committed filename this package looks for.
const File = "pilldrops.json"

// Version is bumped when the shape changes incompatibly.
const Version = 1

// Resources mirrors the optional resources block. Zero means not specified.
type Resources struct{ VCPU, MemoryMB, StorageMB int }

// Config is the parsed, validated form of pilldrops.json.
type Config struct {
	Version   int
	Template  string
	Resources Resources
	Setup     []string
	Start     string
	Port      int
	Env       []string // names only, never values
}

// rawConfig mirrors the JSON shape. Env stays json.RawMessage so a secret value
// written there by mistake is never decoded into something a log could format.
type rawConfig struct {
	Version   *int            `json:"version"`
	Template  *string         `json:"template"`
	Resources *rawResources   `json:"resources"`
	Setup     *[]string       `json:"setup"`
	Start     *string         `json:"start"`
	Port      *int            `json:"port"`
	Env       json.RawMessage `json:"env"`
}

type rawResources struct {
	VCPU      *int `json:"vcpu"`
	MemoryMB  *int `json:"memory_mb"`
	StorageMB *int `json:"storage_mb"`
}

// Parse parses and validates raw as pilldrops.json. Every error names File.
func Parse(raw string, knownTemplateIDs []string) (Config, error) {
	var r rawConfig
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Config{}, fmt.Errorf("%s isn't valid JSON: %w", File, err)
	}

	version := 1
	if r.Version != nil {
		version = *r.Version
	}
	if version != Version {
		return Config{}, fmt.Errorf("%s has version %d, which this server doesn't understand", File, version)
	}

	c := Config{Version: Version}

	if r.Template != nil {
		id := *r.Template
		found := false
		for _, known := range knownTemplateIDs {
			if known == id {
				found = true
				break
			}
		}
		if !found {
			return Config{}, fmt.Errorf("%s names the template %q, which isn't in the catalog", File, id)
		}
		c.Template = id
	}

	// env is names only; the file is committed, so values would put credentials
	// in git. The error must never echo the rejected value.
	if r.Env != nil {
		var names []string
		if err := json.Unmarshal(r.Env, &names); err != nil {
			return Config{}, fmt.Errorf(
				"%s: \"env\" must be a list of variable NAMES, not values — this file is committed to your repository", File)
		}
		c.Env = names
	}

	if r.Setup != nil {
		c.Setup = *r.Setup
	}

	if r.Start != nil {
		c.Start = *r.Start
	}

	if r.Port != nil {
		p := *r.Port
		if p <= 0 || p > 65535 {
			return Config{}, fmt.Errorf("%s: \"port\" should be a number between 1 and 65535", File)
		}
		c.Port = p
	}

	if r.Resources != nil {
		if r.Resources.VCPU != nil {
			if *r.Resources.VCPU <= 0 {
				return Config{}, fmt.Errorf("%s: \"resources.vcpu\" should be a positive number", File)
			}
			c.Resources.VCPU = *r.Resources.VCPU
		}
		if r.Resources.MemoryMB != nil {
			if *r.Resources.MemoryMB <= 0 {
				return Config{}, fmt.Errorf("%s: \"resources.memory_mb\" should be a positive number", File)
			}
			c.Resources.MemoryMB = *r.Resources.MemoryMB
		}
		if r.Resources.StorageMB != nil {
			if *r.Resources.StorageMB <= 0 {
				return Config{}, fmt.Errorf("%s: \"resources.storage_mb\" should be a positive number", File)
			}
			c.Resources.StorageMB = *r.Resources.StorageMB
		}
	}

	return c, nil
}

// FindIn returns the index of the root-level pilldrops.json among paths.
// A nested one belongs to a dependency and is ignored.
func FindIn(paths []string) (int, bool) {
	for i, p := range paths {
		if p == File {
			return i, true
		}
	}
	return 0, false
}
