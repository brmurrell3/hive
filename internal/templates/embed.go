// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package templates

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:init all:ci-pipeline all:research-team all:content-pipeline all:data-processor all:monitor
var embeddedFS embed.FS

// templateDescriptions maps template names to one-line descriptions.
var templateDescriptions = map[string]string{
	"ci-pipeline":      "Three-agent CI/CD pipeline: code review, testing, and security scanning",
	"research-team":    "Two-agent research team: topic research and findings synthesis",
	"content-pipeline": "Three-agent content pipeline: drafting, editing, and fact-checking",
	"data-processor":   "Three-agent data pipeline: ingestion, transformation, and validation",
	"monitor":          "Two-agent monitoring system: target watching and alerting",
}

// ListTemplates returns available template names with descriptions.
func ListTemplates() map[string]string {
	return templateDescriptions
}

// TemplateExists returns true if the named template exists in the embedded FS.
func TemplateExists(name string) bool {
	_, ok := templateDescriptions[name]
	return ok
}

// CopyTemplate copies all files from the named template into the target directory.
// It preserves directory structure and file permissions.
func CopyTemplate(name, targetDir string) error {
	return copyEmbeddedDir(embeddedFS, name, targetDir)
}

func copyEmbeddedDir(fsys embed.FS, srcDir, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}
	return fs.WalkDir(fsys, srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Compute the relative path from srcDir.
		if len(path) <= len(srcDir) {
			return nil // skip the root dir entry itself
		}
		rel := path[len(srcDir)+1:]
		target := filepath.Join(dstDir, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}

		// Preserve executable permission for .sh files.
		perm := os.FileMode(0644)
		if strings.HasSuffix(path, ".sh") {
			perm = 0755
		}
		return os.WriteFile(target, data, perm)
	})
}
