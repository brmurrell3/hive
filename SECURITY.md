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

## Verifying Release Artifacts

Every release includes cryptographic verification artifacts. We recommend verifying downloads before installation.

### SHA-256 Checksums

Each release includes a `checksums.txt` file with SHA-256 hashes for all archives:

```bash
curl -fSLO https://github.com/brmurrell3/hive/releases/download/vX.Y.Z/checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

### Cosign Signature Verification

Release checksums are signed using [Sigstore cosign](https://docs.sigstore.dev/cosign/system_config/installation/) with keyless OIDC (no private key — identity is tied to the GitHub Actions workflow):

```bash
curl -fSLO https://github.com/brmurrell3/hive/releases/download/vX.Y.Z/checksums.txt
curl -fSLO https://github.com/brmurrell3/hive/releases/download/vX.Y.Z/checksums.txt.sig
curl -fSLO https://github.com/brmurrell3/hive/releases/download/vX.Y.Z/checksums.txt.pem

cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'github\.com/brmurrell3/hive'
```

### SLSA Provenance

Releases include [SLSA](https://slsa.dev/) build provenance attestations, verifiable with the GitHub CLI:

```bash
gh attestation verify <artifact-file> --repo brmurrell3/hive
```

### SBOMs

CycloneDX Software Bills of Materials are attached to each release as `*.sbom.json` files. These can be used with vulnerability scanners like [Grype](https://github.com/anchore/grype):

```bash
grype sbom:./hive-0.9.0-linux-amd64.sbom.json
```

## Security Considerations

Hive orchestrates LLM agents across heterogeneous hardware. Key security areas:

- **Agent isolation:** Tier 1 agents run in Firecracker microVMs with minimal attack surface
- **NATS authentication:** Token-based auth for all NATS connections
- **Join tokens:** SHA-256 hashed, single-use tokens for node registration
- **RBAC:** Role-based access control (admin/operator/viewer) for all control plane operations
- **State file permissions:** `state.db` and auth tokens written with restrictive file modes (0600/0700)
