# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Hive, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please use [GitHub's private vulnerability reporting](https://github.com/brmurrell3/hive/security/advisories/new). Include:

- A description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

We will acknowledge receipt within 48 hours and aim to provide a fix or mitigation within 7 days for critical issues.

## Supported Versions

| Version | Supported |
|---------|-----------|
| latest  | Yes       |

## Security Considerations

Hive orchestrates LLM agents across heterogeneous hardware. Key security areas:

- **Agent isolation:** Tier 1 agents run in Firecracker microVMs with minimal attack surface
- **NATS authentication:** Token-based auth for all NATS connections
- **Join tokens:** SHA-256 hashed, single-use tokens for node registration
- **RBAC:** Role-based access control (admin/operator/viewer) for all control plane operations
- **State file permissions:** `state.db` and auth tokens written with restrictive file modes (0600/0700)
