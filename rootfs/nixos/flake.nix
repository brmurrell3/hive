{
  description = "Hive NixOS rootfs for Firecracker VMs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-24.11";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;

      # Build a NixOS system configuration for a given system architecture.
      mkNixosSystem = system: nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          ./configuration.nix
        ];
      };
    in
    {
      packages = forAllSystems (system:
        let
          nixosSystem = mkNixosSystem system;
        in
        {
          # Full ext4 rootfs image ready for Firecracker.
          rootfs = nixosSystem.config.system.build.rootfsImage;

          # Kernel binary (vmlinux) suitable for Firecracker direct boot.
          # The vmlinux file is at: result/bzImage (or result/vmlinux depending on config).
          kernel = nixosSystem.config.boot.kernelPackages.kernel;

          # The complete NixOS system closure (useful for debugging).
          toplevel = nixosSystem.config.system.build.toplevel;
        }
      );

      # `nix build` with no fragment produces the rootfs image.
      defaultPackage = forAllSystems (system: self.packages.${system}.rootfs);

      # Expose the full NixOS configuration for inspection / extension.
      nixosConfigurations = builtins.listToAttrs (map (system: {
        name = "hive-vm-${system}";
        value = mkNixosSystem system;
      }) supportedSystems);
    };
}
