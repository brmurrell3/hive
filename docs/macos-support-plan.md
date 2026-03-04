# macOS Support Plan

Hive currently runs on Linux only. This document outlines what is required to officially support macOS as a Tier 1 control plane host, and the intended implementation approach.

## Current State

macOS is supported for development and testing only, via `HIVE_TEST_FIRECRACKER=mock`. No real VM workloads run on macOS today.

## Target State

A macOS host running `hived` with full VM-based agent support, configured declaratively via a nix-darwin module — the same experience as the existing NixOS module.

## Blockers

### 1. Hypervisor Backend (largest change)

Firecracker is Linux/KVM-only. The macOS replacement is **Apple Virtualization.framework**, accessed from Go via [`github.com/Code-Hex/vz`](https://github.com/Code-Hex/vz).

The `Hypervisor` interface in `internal/vm/manager.go` already abstracts VM operations cleanly. A new `AppleVZHypervisor` implementation would slot in alongside `FirecrackerHypervisor` with no changes to the manager or the rest of the stack.

Apple VZ supports Linux guests, virtio-net, virtio-vsock, and uses HVF (Hypervisor.framework) for near-native performance.

**Entitlement requirement:** the `hived` binary must be code-signed with `com.apple.security.virtualization`. This is handled in the Nix derivation (see [Delivery](#delivery) below).

### 2. vsock (`internal/vm/vsock_linux.go`)

Tagged `//go:build linux`. The current implementation uses Firecracker's UDS-based vsock forwarding. Apple VZ exposes virtio-vsock through its own API (`VZVirtioSocketDevice`), which is different but achieves the same result.

**Plan:** add `vsock_darwin.go` implementing the same `VsockForwarder` interface using the Apple VZ socket device.

### 3. Network Policy — nftables → pf (`internal/vm/network.go`)

`GenerateNftables` / `CleanupNftables` produce rules for `nft -f`. macOS uses **pf** (packet filter), which has different syntax and is driven via `pfctl` with per-VM anchors.

**Plan:**
- Extract a `NetworkPolicyEnforcer` interface from `network.go`
- `network_linux.go` — existing nftables implementation
- `network_darwin.go` — pf/pfctl implementation using named anchors (`hive/<agentID>`)

Apple VZ provides `VZNATNetworkDeviceAttachment` for NAT networking, which gives baseline isolation. pf anchors add the same egress/ingress policy enforcement that nftables provides on Linux.

### 4. System Resource Monitoring (`internal/production/sysresources_linux.go`)

Reads `/proc/meminfo` and `/proc/stat`. Already build-tag isolated.

**Plan:** add `sysresources_darwin.go` using `syscall.Sysctl` / `host_statistics64` via `golang.org/x/sys/unix`. Straightforward port.

### 5. Process Monitoring (`internal/production/process_linux.go`)

Reads `/proc/<pid>/comm` to verify a PID belongs to the hypervisor process.

**Plan:** add `process_darwin.go` using `sysctl(KERN_PROC, KERN_PROC_PID)` to check the process name. Minor effort.

### 6. Sidecar Reaper (`cmd/hive-sidecar/reaper_linux.go`)

Linux-only child process reaping via `prctl(PR_SET_CHILD_SUBREAPER)`.

**Plan:** add `reaper_darwin.go` as a no-op stub. macOS handles zombie reaping at the process level and the sidecar does not manage child processes in the same way.

### 7. Rootfs Image Pipeline

Firecracker uses ext4 disk images; `mkfs.ext4 -d` is unavailable on macOS. Apple VZ can boot Linux guests from a raw disk image — the guest OS and kernel format remain the same, only the image creation tooling changes.

**Plan:** update `rootfs/build-rootfs.sh` to detect the host OS. On macOS, use QEMU or Docker (both available via Homebrew/nix-darwin) to create the ext4 image cross-platform. The NixOS rootfs images built by the flake are unaffected.

## Summary of New/Changed Files

| File | Change |
|---|---|
| `internal/vm/hypervisor_darwin.go` | `AppleVZHypervisor` implementation via `github.com/Code-Hex/vz` |
| `internal/vm/vsock_darwin.go` | Apple VZ virtio-vsock forwarder |
| `internal/vm/network.go` | Extract `NetworkPolicyEnforcer` interface |
| `internal/vm/network_linux.go` | Existing nftables logic, renamed |
| `internal/vm/network_darwin.go` | pf/pfctl implementation |
| `internal/production/sysresources_darwin.go` | Memory/CPU stats via sysctl |
| `internal/production/process_darwin.go` | Process name check via sysctl |
| `cmd/hive-sidecar/reaper_darwin.go` | No-op stub |
| `rootfs/build-rootfs.sh` | Cross-platform image creation |
| `nix/options.nix` | Shared module options (extracted from `module.nix`) |
| `nix/module.nix` | Updated to import `options.nix` |
| `nix/module-darwin.nix` | nix-darwin module (see below) |

## Delivery

### nix-darwin Module

The macOS module mirrors the NixOS module exactly, with `launchd.daemons` replacing `systemd.services`. Shared option definitions (cluster root, user, agents, etc.) are extracted into `nix/options.nix` and imported by both modules.

```nix
# nix-darwin usage — identical to NixOS
services.hived = {
  enable = true;
  clusterRoot = "/Users/deploy/hive-cluster";
  openFirewall = true;
};
```

Code signing is handled in the flake's package derivation via a `postFixup` phase:

```nix
postFixup = ''
  codesign --entitlements ${entitlementsFile} \
    --sign - $out/bin/hived
'';
```

where `entitlementsFile` grants `com.apple.security.virtualization`. This makes signing automatic and reproducible — any machine building from the flake gets a correctly signed binary.

### Homebrew Tap (future, optional)

A Homebrew formula is a lower-barrier entry point for users not on Nix. It would install the pre-signed binary and a launchd plist template for `brew services start hived`. This is a subset of what nix-darwin provides (no declarative agent management, no user/group setup) and is lower priority.

## Non-Goals

- macOS support for Tier 3 firmware agents (those are embedded devices)
- Running the Hive sidecar VM image on macOS (the guest OS is always Linux)
- Supporting macOS versions before 13 (Ventura), which is the minimum for the Apple VZ APIs used here
