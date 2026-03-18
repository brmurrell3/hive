// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package sidecar

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// protocolTemplate is the Go template for the HIVE.md file written into each
// agent workspace on startup. It contains the sidecar HTTP API reference and
// agent-specific identity so that LLM-based agents can self-discover their
// capabilities and communicate with the cluster.
const protocolTemplate = `# Hive Agent Protocol

You are agent **{{.AgentID}}**{{if .TeamID}} on team **{{.TeamID}}**{{end}} (tier: {{.Tier}}).
Your sidecar API is at **{{.SidecarURL}}**.

## Your Capabilities
{{if .Capabilities}}
{{range .Capabilities}}- **{{.Name}}**: {{.Description}}
{{- if .Inputs}}
  Inputs:{{range .Inputs}} ` + "`" + `{{.Name}}` + "`" + ` ({{.Type}}{{if .Required}}, required{{end}}){{end}}
{{- end}}
{{- if .Outputs}}
  Outputs:{{range .Outputs}} ` + "`" + `{{.Name}}` + "`" + ` ({{.Type}}){{end}}
{{- end}}
{{end}}
{{- else}}
No capabilities registered.
{{end}}

## Sidecar HTTP API

All endpoints except /health require ` + "`" + `Authorization: Bearer <token>` + "`" + ` if auth is configured.

### Discovery

` + "`" + `GET /health` + "`" + ` — Health status (no auth)
` + "```" + `json
{"sidecar": "healthy", "runtime": "healthy", "uptime_seconds": 42}
` + "```" + `

` + "`" + `GET /capabilities` + "`" + ` — Your registered capabilities

` + "`" + `GET /team/capabilities` + "`" + ` — All capabilities across the cluster
` + "```" + `json
[{"name": "...", "agent_id": "...", "team_id": "...", "description": "..."}]
` + "```" + `

### Invoking Other Agents

` + "`" + `POST /capabilities/{name}/invoke` + "`" + ` — Invoke YOUR OWN capability locally
` + "```" + `json
{"inputs": {"key": "value"}, "timeout": "30s"}
` + "```" + `

` + "`" + `POST /capabilities/{name}/invoke-remote` + "`" + ` — Invoke a capability on ANOTHER agent
` + "```" + `json
{"target": "agent-id", "inputs": {"key": "value"}, "timeout": "30s"}
` + "```" + `

**Response format (both endpoints):**
` + "```" + `json
{
  "status": "success|error|timeout",
  "outputs": {"result": "..."},
  "duration_ms": 150,
  "error": {"code": "...", "message": "...", "retryable": false}
}
` + "```" + `

Error codes: NOT_FOUND, HANDLER_ERROR, HANDLER_TIMEOUT, SERVICE_OVERLOADED, AGENT_OFFLINE.

## Workflow

1. Call ` + "`" + `GET /team/capabilities` + "`" + ` to discover agents and their tools
2. Call ` + "`" + `POST /capabilities/{cap}/invoke-remote` + "`" + ` with the target agent_id
3. Collect outputs and chain into the next invocation

Do NOT spawn sub-processes to do work that existing agents can handle — invoke them via the sidecar API.

## Environment Variables

| Variable | Value |
|----------|-------|
| HIVE_AGENT_ID | {{.AgentID}} |
| HIVE_TEAM_ID | {{.TeamID}} |
| HIVE_SIDECAR_URL | {{.SidecarURL}} |
| HIVE_WORKSPACE | {{.Workspace}} |

Also available: HIVE_SOUL (SOUL.md contents), HIVE_MEMORY (MEMORY.md contents), HIVE_PROTOCOL (this document).

## Workspace Files

| File | Purpose |
|------|---------|
| SOUL.md | Agent system prompt / personality |
| MEMORY.md | Agent memory (hot-reloaded via NATS) |
| HIVE.md | This protocol reference (auto-generated) |
| .hive-metadata.json | Agent identity and startup info |
`

// protocolData holds the template parameters for rendering HIVE.md.
type protocolData struct {
	AgentID      string
	TeamID       string
	Tier         string
	SidecarURL   string
	Workspace    string
	Capabilities []Capability
}

// renderProtocolDoc renders the HIVE.md protocol reference with this agent's
// identity and capabilities filled in.
func (s *Sidecar) renderProtocolDoc() (string, error) {
	tmpl, err := template.New("protocol").Parse(protocolTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing protocol template: %w", err)
	}

	sidecarURL := "http://localhost:9100"
	if s.config.HTTPAddr != "" && s.config.HTTPAddr != ":0" {
		addr := s.config.HTTPAddr
		if strings.HasPrefix(addr, ":") {
			addr = "localhost" + addr
		}
		scheme := "http"
		if s.config.TLSCertFile != "" {
			scheme = "https"
		}
		sidecarURL = scheme + "://" + addr
	}

	tier := s.config.Tier
	if tier == "" {
		tier = "vm"
	}

	data := protocolData{
		AgentID:      s.agentID,
		TeamID:       s.teamID,
		Tier:         tier,
		SidecarURL:   sidecarURL,
		Workspace:    s.config.WorkspacePath,
		Capabilities: s.capabilities,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing protocol template: %w", err)
	}

	return buf.String(), nil
}

// writeProtocolDoc renders and writes the HIVE.md protocol reference to the
// agent workspace and any additional ProtocolDocPaths. Returns nil without
// action if no paths are configured.
func (s *Sidecar) writeProtocolDoc() error {
	paths := s.protocolDocDirs()
	if len(paths) == 0 {
		return nil
	}

	content, err := s.renderProtocolDoc()
	if err != nil {
		return err
	}

	const maxProtocolSize = 256 * 1024 // 256KB
	if len(content) > maxProtocolSize {
		return fmt.Errorf("rendered protocol doc too large: %d bytes", len(content))
	}

	var firstErr error
	for _, dir := range paths {
		hivePath := filepath.Join(dir, "HIVE.md")
		if err := writeFileAtomic(hivePath, []byte(content), 0644); err != nil {
			s.logger.Warn("failed to write HIVE.md",
				"path", hivePath,
				"error", err,
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("writing HIVE.md to %s: %w", hivePath, err)
			}
			continue
		}
		s.logger.Info("wrote HIVE.md protocol reference",
			"path", hivePath,
			"size_bytes", len(content),
		)
	}

	return firstErr
}

// cleanupProtocolDoc removes the HIVE.md file from all configured paths.
// Best-effort; errors are logged but not returned. Called during shutdown.
func (s *Sidecar) cleanupProtocolDoc() {
	for _, dir := range s.protocolDocDirs() {
		hivePath := filepath.Join(dir, "HIVE.md")
		if err := os.Remove(hivePath); err != nil && !os.IsNotExist(err) {
			s.logger.Warn("failed to remove HIVE.md", "path", hivePath, "error", err)
		}
	}
}

// protocolDocDirs returns the deduplicated list of directories where HIVE.md
// should be written. WorkspacePath is always included (if set), followed by
// any ProtocolDocPaths that differ from it.
func (s *Sidecar) protocolDocDirs() []string {
	seen := make(map[string]struct{})
	var dirs []string

	add := func(d string) {
		if d == "" {
			return
		}
		clean := filepath.Clean(d)
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		dirs = append(dirs, clean)
	}

	add(s.config.WorkspacePath)
	for _, p := range s.config.ProtocolDocPaths {
		add(p)
	}
	return dirs
}
