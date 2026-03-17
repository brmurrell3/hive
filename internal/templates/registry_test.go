// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package templates

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// writeOverrideFile creates a file under the override directory with the given
// content and returns the full path.
func writeOverrideFile(t *testing.T, overrideDir, name, content string) string {
	t.Helper()
	p := filepath.Join(overrideDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll for override file: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile for override: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// embed.go: ListTemplates / TemplateExists / CopyTemplate
// ---------------------------------------------------------------------------

func TestListTemplates(t *testing.T) {
	t.Parallel()

	templates := ListTemplates()
	if len(templates) != 5 {
		t.Fatalf("expected 5 templates, got %d", len(templates))
	}

	expected := map[string]string{
		"ci-pipeline":      "Three-agent CI/CD pipeline: code review, testing, and security scanning",
		"research-team":    "Two-agent research team: topic research and findings synthesis",
		"content-pipeline": "Three-agent content pipeline: drafting, editing, and fact-checking",
		"data-processor":   "Three-agent data pipeline: ingestion, transformation, and validation",
		"monitor":          "Two-agent monitoring system: target watching and alerting",
	}

	for name, desc := range expected {
		got, ok := templates[name]
		if !ok {
			t.Errorf("missing template %q", name)
			continue
		}
		if got != desc {
			t.Errorf("template %q: got description %q, want %q", name, got, desc)
		}
	}
}

func TestTemplateExists(t *testing.T) {
	t.Parallel()

	existing := []string{
		"ci-pipeline",
		"research-team",
		"content-pipeline",
		"data-processor",
		"monitor",
	}
	for _, name := range existing {
		if !TemplateExists(name) {
			t.Errorf("TemplateExists(%q) = false, want true", name)
		}
	}

	if TemplateExists("nonexistent") {
		t.Error("TemplateExists(\"nonexistent\") = true, want false")
	}
}

func TestCopyTemplate_Success(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()
	if err := CopyTemplate("ci-pipeline", dst); err != nil {
		t.Fatalf("CopyTemplate: %v", err)
	}

	// Verify key files exist.
	for _, relPath := range []string{
		"cluster.yaml",
		"README.md",
		"agents/code-reviewer/manifest.yaml",
		"agents/code-reviewer/entrypoint.sh",
		"agents/test-runner/manifest.yaml",
		"agents/security-scanner/manifest.yaml",
		"teams/ci-pipeline.yaml",
	} {
		full := filepath.Join(dst, relPath)
		if _, err := os.Stat(full); os.IsNotExist(err) {
			t.Errorf("expected file %q to exist after CopyTemplate", relPath)
		}
	}
}

func TestCopyTemplate_Permissions(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()
	if err := CopyTemplate("ci-pipeline", dst); err != nil {
		t.Fatalf("CopyTemplate: %v", err)
	}

	// .sh files should get 0755.
	shFile := filepath.Join(dst, "agents", "code-reviewer", "entrypoint.sh")
	info, err := os.Stat(shFile)
	if err != nil {
		t.Fatalf("Stat %q: %v", shFile, err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("entrypoint.sh perm = %o, want 0755", perm)
	}

	// Non-sh files should get 0644.
	yamlFile := filepath.Join(dst, "agents", "code-reviewer", "manifest.yaml")
	info, err = os.Stat(yamlFile)
	if err != nil {
		t.Fatalf("Stat %q: %v", yamlFile, err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("manifest.yaml perm = %o, want 0644", perm)
	}
}

func TestCopyTemplate_NonexistentTemplate(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()
	err := CopyTemplate("nonexistent-template", dst)
	if err == nil {
		t.Fatal("expected error for nonexistent template, got nil")
	}
}

func TestCopyTemplate_DirectoryStructure(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()
	if err := CopyTemplate("research-team", dst); err != nil {
		t.Fatalf("CopyTemplate: %v", err)
	}

	// agents/ and teams/ subdirs should exist.
	for _, dir := range []string{"agents", "teams"} {
		info, err := os.Stat(filepath.Join(dst, dir))
		if err != nil {
			t.Errorf("expected directory %q to exist: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", dir)
		}
	}
}

// ---------------------------------------------------------------------------
// registry.go: NewRegistry / Render / RenderString / List / DefaultRegistry
// ---------------------------------------------------------------------------

func TestNewRegistry_NoOverrides(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	if reg == nil {
		t.Fatal("NewRegistry returned nil")
	}

	// Should still be able to render embedded templates.
	out, err := reg.RenderString("init/cluster.yaml.tmpl", struct{ ClusterName string }{"test-cluster"})
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}
	if !strings.Contains(out, "test-cluster") {
		t.Error("rendered output does not contain cluster name")
	}
}

func TestNewRegistry_WithOverrides(t *testing.T) {
	t.Parallel()

	overrideDir := t.TempDir()
	writeOverrideFile(t, overrideDir, "init/cluster.yaml.tmpl", "override: {{.ClusterName}}")

	reg := NewRegistry(overrideDir)
	out, err := reg.RenderString("init/cluster.yaml.tmpl", struct{ ClusterName string }{"override-test"})
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}
	if out != "override: override-test" {
		t.Errorf("got %q, want %q", out, "override: override-test")
	}
}

func TestRender_EmbeddedTemplate(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	data := struct{ ClusterName string }{"my-cluster"}
	b, err := reg.Render("init/cluster.yaml.tmpl", data)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	output := string(b)
	if !strings.Contains(output, "my-cluster") {
		t.Error("rendered output does not contain ClusterName value")
	}
	if !strings.Contains(output, "apiVersion: hive/v1") {
		t.Error("rendered output does not contain expected YAML header")
	}
}

func TestRender_NotFound(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	_, err := reg.Render("does/not/exist.tmpl", nil)
	if err == nil {
		t.Fatal("expected error for non-existent template, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not mention 'not found'", err)
	}
}

func TestRender_TemplateParseError(t *testing.T) {
	t.Parallel()

	overrideDir := t.TempDir()
	// Malformed template syntax should cause a parse error.
	writeOverrideFile(t, overrideDir, "bad.tmpl", "{{.Unclosed")

	reg := NewRegistry(overrideDir)
	_, err := reg.Render("bad.tmpl", nil)
	if err == nil {
		t.Fatal("expected error for malformed template, got nil")
	}
	if !strings.Contains(err.Error(), "parsing") {
		t.Errorf("error %q does not mention 'parsing'", err)
	}
}

func TestRenderString(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	data := struct{ ClusterName string }{"string-test"}
	s, err := reg.RenderString("init/cluster.yaml.tmpl", data)
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}
	if !strings.Contains(s, "string-test") {
		t.Error("RenderString output does not contain expected cluster name")
	}

	// Verify it matches Render output.
	b, err := reg.Render("init/cluster.yaml.tmpl", data)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if s != string(b) {
		t.Error("RenderString and Render outputs differ")
	}
}

func TestRender_OverrideTakesPrecedence(t *testing.T) {
	t.Parallel()

	overrideDir := t.TempDir()
	writeOverrideFile(t, overrideDir, "init/cluster.yaml.tmpl", "custom: {{.ClusterName}}")

	reg := NewRegistry(overrideDir)
	out, err := reg.RenderString("init/cluster.yaml.tmpl", struct{ ClusterName string }{"precedence"})
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}

	// Must come from the override, not the embedded version.
	if out != "custom: precedence" {
		t.Errorf("got %q, want %q", out, "custom: precedence")
	}
}

func TestRender_CacheHit(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	data := struct{ ClusterName string }{"cache-test"}

	out1, err := reg.RenderString("init/cluster.yaml.tmpl", data)
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}

	out2, err := reg.RenderString("init/cluster.yaml.tmpl", data)
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}

	if out1 != out2 {
		t.Error("cached render returned different result")
	}
}

func TestRender_CacheInvalidation(t *testing.T) {
	t.Parallel()

	overrideDir := t.TempDir()
	overridePath := writeOverrideFile(t, overrideDir, "init/cluster.yaml.tmpl", "v1: {{.ClusterName}}")

	reg := NewRegistry(overrideDir)
	out1, err := reg.RenderString("init/cluster.yaml.tmpl", struct{ ClusterName string }{"test"})
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}
	if out1 != "v1: test" {
		t.Fatalf("first render: got %q, want %q", out1, "v1: test")
	}

	// Modify the override file. We need to ensure the mod time actually
	// changes (filesystem granularity may be 1s), so we explicitly advance it.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(overridePath, []byte("v2: {{.ClusterName}}"), 0o644); err != nil {
		t.Fatalf("rewrite override: %v", err)
	}
	// Force a distinct mod time in case the filesystem has coarse granularity.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(overridePath, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	out2, err := reg.RenderString("init/cluster.yaml.tmpl", struct{ ClusterName string }{"test"})
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	if out2 != "v2: test" {
		t.Errorf("after invalidation: got %q, want %q", out2, "v2: test")
	}
}

func TestList_InitPrefix(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	names := reg.List("init")

	if len(names) == 0 {
		t.Fatal("List(\"init\") returned no results")
	}

	// All returned names should start with "init/".
	for _, n := range names {
		if !strings.HasPrefix(n, "init/") {
			t.Errorf("unexpected name %q (does not start with \"init/\")", n)
		}
	}

	// We know the embedded init dir has these files.
	expectedFiles := []string{
		"init/cluster.yaml.tmpl",
		"init/agent-manifest.yaml.tmpl",
		"init/team.yaml.tmpl",
		"init/setup-pi.sh.tmpl",
	}
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	for _, f := range expectedFiles {
		if !nameSet[f] {
			t.Errorf("expected %q in List(\"init\") results", f)
		}
	}
}

func TestList_TemplatePrefix(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	names := reg.List("ci-pipeline")

	if len(names) == 0 {
		t.Fatal("List(\"ci-pipeline\") returned no results")
	}

	for _, n := range names {
		if !strings.HasPrefix(n, "ci-pipeline/") {
			t.Errorf("unexpected name %q (does not start with \"ci-pipeline/\")", n)
		}
	}

	// Verify key files are present.
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	for _, f := range []string{
		"ci-pipeline/cluster.yaml",
		"ci-pipeline/README.md",
		"ci-pipeline/teams/ci-pipeline.yaml",
		"ci-pipeline/agents/code-reviewer/entrypoint.sh",
	} {
		if !nameSet[f] {
			t.Errorf("expected %q in List results", f)
		}
	}
}

func TestList_EmptyPrefix(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	names := reg.List(".")

	if len(names) == 0 {
		t.Fatal("List(\".\") returned no results")
	}

	// Should include files from multiple subdirectories.
	hasInit := false
	hasCIPipeline := false
	for _, n := range names {
		if strings.HasPrefix(n, "init/") {
			hasInit = true
		}
		if strings.HasPrefix(n, "ci-pipeline/") {
			hasCIPipeline = true
		}
	}
	if !hasInit {
		t.Error("List(\".\") missing init/ templates")
	}
	if !hasCIPipeline {
		t.Error("List(\".\") missing ci-pipeline/ templates")
	}
}

func TestList_NonexistentPrefix(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	names := reg.List("nonexistent-prefix")

	if len(names) != 0 {
		t.Errorf("expected empty slice for nonexistent prefix, got %v", names)
	}
}

func TestList_Deduplicates(t *testing.T) {
	t.Parallel()

	overrideDir := t.TempDir()
	// Create an override file that shadows an embedded file.
	writeOverrideFile(t, overrideDir, "init/cluster.yaml.tmpl", "override content")

	reg := NewRegistry(overrideDir)
	names := reg.List("init")

	// Count occurrences of the shadowed file.
	count := 0
	for _, n := range names {
		if n == "init/cluster.yaml.tmpl" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("init/cluster.yaml.tmpl appears %d times, want 1", count)
	}

	// Also verify the list is sorted.
	if !sort.StringsAreSorted(names) {
		t.Errorf("List result is not sorted: %v", names)
	}
}

func TestDefaultRegistry(t *testing.T) {
	t.Parallel()

	reg := DefaultRegistry()
	if reg == nil {
		t.Fatal("DefaultRegistry returned nil")
	}

	// It should be able to render an embedded template.
	out, err := reg.RenderString("init/cluster.yaml.tmpl", struct{ ClusterName string }{"default-reg"})
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}
	if !strings.Contains(out, "default-reg") {
		t.Error("DefaultRegistry cannot render embedded templates")
	}
}

func TestConcurrentRender(t *testing.T) {
	t.Parallel()

	reg := NewRegistry("")
	const goroutines = 20

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			data := struct{ ClusterName string }{ClusterName: "concurrent"}
			out, err := reg.RenderString("init/cluster.yaml.tmpl", data)
			if err != nil {
				errs <- err
				return
			}
			if !strings.Contains(out, "concurrent") {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent render error: %v", err)
	}
}
