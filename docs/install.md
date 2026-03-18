# Installing Hive

## macOS

### Homebrew (recommended)

```bash
brew install brmurrell3/tap/hive
```

This installs `hived`, `hivectl`, `hive-agent`, and `hive-sidecar`. Upgrades are handled by `brew upgrade hive`.

### Manual download

Download the archive for your architecture from the [latest release](https://github.com/brmurrell3/hive/releases/latest):

```bash
# Apple Silicon
curl -fSL -o hive.tar.gz https://github.com/brmurrell3/hive/releases/latest/download/hive-0.9.0-darwin-arm64.tar.gz

# Intel
curl -fSL -o hive.tar.gz https://github.com/brmurrell3/hive/releases/latest/download/hive-0.9.0-darwin-amd64.tar.gz

tar xzf hive.tar.gz
sudo mv hived hivectl hive-agent hive-sidecar /usr/local/bin/
```

## Linux

### Install script (Ubuntu/Debian)

The install script downloads binaries, verifies checksums, sets up a system user, installs the systemd unit, and optionally downloads VM images (kernel + rootfs):

```bash
curl -fsSL https://raw.githubusercontent.com/brmurrell3/hive/main/scripts/install.sh | sudo bash
```

Supported distributions: Ubuntu 22.04/24.04, Debian 12. Supported architectures: amd64, arm64.

Options:

```bash
# Install a specific version
curl -fsSL ... | sudo bash -s -- --version v0.9.0

# Dry run (show what would be done)
curl -fsSL ... | sudo bash -s -- --dry-run

# Skip kernel/rootfs image downloads
curl -fsSL ... | sudo bash -s -- --skip-images

# Non-interactive (accept defaults)
curl -fsSL ... | sudo bash -s -- --yes
```

After installation:

```bash
sudo systemctl enable --now hived
sudo journalctl -u hived -f
```

### Manual download

```bash
# x86_64
curl -fSL -o hive.tar.gz https://github.com/brmurrell3/hive/releases/latest/download/hive-0.9.0-linux-amd64.tar.gz

# ARM64
curl -fSL -o hive.tar.gz https://github.com/brmurrell3/hive/releases/latest/download/hive-0.9.0-linux-arm64.tar.gz

tar xzf hive.tar.gz
sudo mv hived hivectl hive-agent hive-sidecar /usr/local/bin/
```

### systemd setup (manual)

If you downloaded binaries manually and want systemd integration:

```bash
sudo ./deploy/systemd/hive-setup.sh
```

This creates the `hive` user/group, data directories, and installs the systemd unit file. Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now hived
```

## NixOS

### Flake module (recommended)

Add Hive as a flake input and enable the NixOS module:

```nix
# /etc/nixos/flake.nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    hive.url = "github:brmurrell3/hive";
  };

  outputs = { nixpkgs, hive, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        ./configuration.nix
        hive.nixosModules.default
        {
          services.hived = {
            enable = true;
            clusterRoot = "/var/lib/hive/cluster";
            openFirewall = true;
          };
        }
      ];
    };
  };
}
```

This sets up hived as a systemd service with proper user isolation and firewall rules.

### Nix run (try without installing)

```bash
nix run github:brmurrell3/hive#hivectl -- version
```

### Nix profile (user install)

```bash
nix profile install github:brmurrell3/hive#hived
nix profile install github:brmurrell3/hive#hivectl
nix profile install github:brmurrell3/hive#hive-agent
```

## From Source

Requires [Go 1.25+](https://go.dev/dl/).

```bash
git clone https://github.com/brmurrell3/hive && cd hive
make build
```

Binaries are written to `./bin/`. To install system-wide:

```bash
sudo cp bin/hived bin/hivectl bin/hive-agent bin/hive-sidecar /usr/local/bin/
```

### Cross-compilation

```bash
make build-linux-amd64    # Linux x86_64
make build-linux-arm64    # Linux ARM64 (Raspberry Pi, etc.)
make build-darwin-amd64   # macOS Intel
make build-all            # All of the above + native
```

## VM Images (Linux only)

Firecracker VM isolation requires a kernel and rootfs image. These are included automatically by the install script. For manual or source installs:

```bash
# Download a Firecracker-compatible kernel
make download-kernel

# Build an Alpine rootfs (requires Docker)
make rootfs

# Or build a NixOS rootfs (requires Nix)
cd rootfs/nixos && nix build .#rootfs && nix build .#kernel
```

Pre-built images are also attached to each [GitHub release](https://github.com/brmurrell3/hive/releases/latest).

## Air-gapped / Offline

For environments without internet access:

1. On a connected machine, download the release archive, kernel, and rootfs from the [releases page](https://github.com/brmurrell3/hive/releases/latest)
2. Transfer the files to the target machine
3. Extract binaries to `/usr/local/bin/`
4. Set `imageURL: file:///path/to/rootfs.ext4` in `cluster.yaml` for local rootfs
5. Set `kernelPath` and `rootfsPath` in `cluster.yaml` to the local paths

## Verifying Downloads

All release artifacts include SHA-256 checksums and cosign signatures:

```bash
# Verify checksum
curl -fSLO https://github.com/brmurrell3/hive/releases/latest/download/checksums.txt
sha256sum --check --ignore-missing checksums.txt

# Verify cosign signature (requires cosign)
curl -fSLO https://github.com/brmurrell3/hive/releases/latest/download/checksums.txt.sig
curl -fSLO https://github.com/brmurrell3/hive/releases/latest/download/checksums.txt.pem
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'github\.com/brmurrell3/hive'

# Verify SLSA provenance (requires gh CLI)
gh attestation verify hive-0.9.0-linux-amd64.tar.gz --repo brmurrell3/hive
```

## Uninstall

### Homebrew

```bash
brew uninstall hive
brew untap brmurrell3/tap
```

### Linux (install script)

```bash
sudo systemctl stop hived
sudo systemctl disable hived
sudo rm /etc/systemd/system/hived.service
sudo rm /usr/local/bin/{hived,hivectl,hive-agent,hive-sidecar}
sudo userdel hive
sudo rm -rf /var/lib/hive /var/log/hive
```

### NixOS

Remove the module from your configuration and run `sudo nixos-rebuild switch`.
