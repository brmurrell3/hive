// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package templates manages Go text/template lookup with embedded defaults and filesystem overrides.
package templates

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"text/template"
	"time"
)

// cacheEntry holds a parsed template alongside the modification time of the
// override file it was loaded from.  For embedded-only templates the modTime
// is the zero value.
type cacheEntry struct {
	tmpl    *template.Template
	modTime time.Time // mod time of override file (zero if embedded)
}

// TemplateRegistry manages template lookup with embedded defaults and filesystem overrides.
// At runtime, it checks the override directory first (e.g., ~/.hive/templates/),
// then falls back to the embedded templates shipped in the binary.
type TemplateRegistry struct {
	embedded  fs.FS
	overrides string // filesystem path for user overrides (may be empty)
	mu        sync.RWMutex
	cache     map[string]*cacheEntry
}

// NewRegistry creates a TemplateRegistry with the embedded templates and
// an optional override directory. If overrides is empty, only embedded
// templates are used.
func NewRegistry(overrides string) *TemplateRegistry {
	return &TemplateRegistry{
		embedded:  embeddedFS,
		overrides: overrides,
		cache:     make(map[string]*cacheEntry),
	}
}

// Render looks up the template by name and executes it with the given data.
// The name should be a path relative to the templates root (e.g.,
// "init/cluster.yaml.tmpl"). Override files take precedence over embedded.
func (r *TemplateRegistry) Render(name string, data interface{}) ([]byte, error) {
	tmpl, err := r.load(name)
	if err != nil {
		return nil, fmt.Errorf("loading template %q: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing template %q: %w", name, err)
	}
	return buf.Bytes(), nil
}

// RenderString is a convenience wrapper that returns the result as a string.
func (r *TemplateRegistry) RenderString(name string, data interface{}) (string, error) {
	b, err := r.Render(name, data)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// List returns all template names matching the given prefix.
func (r *TemplateRegistry) List(prefix string) []string {
	seen := make(map[string]bool)
	var names []string

	// Collect from overrides first.
	if r.overrides != "" {
		overrideRoot := filepath.Join(r.overrides, prefix)
		_ = filepath.WalkDir(overrideRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(r.overrides, path)
			if !seen[rel] {
				seen[rel] = true
				names = append(names, rel)
			}
			return nil
		})
	}

	// Collect from embedded.
	_ = fs.WalkDir(r.embedded, prefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !seen[path] {
			seen[path] = true
			names = append(names, path)
		}
		return nil
	})

	sort.Strings(names)
	return names
}

// load reads the template content (override or embedded) and parses it.
// Results are cached for subsequent calls.  When a filesystem override file
// has been modified since the cached version was loaded, the cache entry is
// invalidated and the template is re-parsed.
func (r *TemplateRegistry) load(name string) (*template.Template, error) {
	// Check if override file exists and get its mod time.
	var overrideModTime time.Time
	if r.overrides != "" {
		overridePath := filepath.Join(r.overrides, name)
		if info, err := os.Stat(overridePath); err == nil {
			overrideModTime = info.ModTime()
		}
	}

	r.mu.RLock()
	if entry, ok := r.cache[name]; ok {
		// Cache hit: check if the override file has changed.
		if entry.modTime.Equal(overrideModTime) {
			r.mu.RUnlock()
			return entry.tmpl, nil
		}
		// Override file changed — invalidate and reload below.
	}
	r.mu.RUnlock()

	content, err := r.readTemplate(name)
	if err != nil {
		return nil, err
	}

	t, err := template.New(name).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parsing template %q: %w", name, err)
	}

	r.mu.Lock()
	r.cache[name] = &cacheEntry{tmpl: t, modTime: overrideModTime}
	r.mu.Unlock()

	return t, nil
}

// readTemplate reads template content, checking overrides first, then embedded.
func (r *TemplateRegistry) readTemplate(name string) ([]byte, error) {
	// Check override directory first.
	if r.overrides != "" {
		overridePath := filepath.Join(r.overrides, name)
		if data, err := os.ReadFile(overridePath); err == nil {
			return data, nil
		}
	}

	// Fall back to embedded.
	data, err := fs.ReadFile(r.embedded, name)
	if err != nil {
		return nil, fmt.Errorf("template %q not found in overrides or embedded", name)
	}
	return data, nil
}

// DefaultRegistry returns a registry using ~/.hive/templates/ as the override directory.
func DefaultRegistry() *TemplateRegistry {
	home, _ := os.UserHomeDir()
	overrides := ""
	if home != "" {
		overrides = filepath.Join(home, ".hive", "templates")
	}
	return NewRegistry(overrides)
}
