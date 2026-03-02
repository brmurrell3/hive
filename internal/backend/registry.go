// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package backend

import (
	"fmt"
	"sort"
	"sync"
)

// Registry manages available backends by name.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend
}

// NewRegistry creates an empty backend registry.
func NewRegistry() *Registry {
	return &Registry{
		backends: make(map[string]Backend),
	}
}

// Register adds a backend to the registry. Returns an error if a backend
// with the same name is already registered.
func (r *Registry) Register(b Backend) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := b.Name()
	if _, exists := r.backends[name]; exists {
		return fmt.Errorf("backend %q already registered", name)
	}
	r.backends[name] = b
	return nil
}

// Get returns a backend by name, or an error if not found.
func (r *Registry) Get(name string) (Backend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	b, ok := r.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %q not registered", name)
	}
	return b, nil
}

// List returns the names of all registered backends.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
