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
      # `nix build` with no fragment produces the rootfs image via `default`.
      packages = forAllSystems (system:
        let
          nixosSystem = mkNixosSystem system;
          pkgs = nixpkgs.legacyPackages.${system};
          linuxKernel = nixosSystem.config.boot.kernelPackages.kernel;
        in
        {
          # Full ext4 rootfs image ready for Firecracker.
          rootfs = nixosSystem.config.system.build.rootfsImage;

          # Uncompressed vmlinux for Firecracker direct boot.
          # Stock NixOS kernel puts vmlinux in the `dev` output;
          # this wrapper copies it so `result/vmlinux` works directly.
          kernel = pkgs.runCommand "hive-vmlinux" {} ''
            mkdir -p $out
            cp ${linuxKernel.dev}/vmlinux $out/vmlinux
          '';

          # The complete NixOS system closure (useful for debugging).
          toplevel = nixosSystem.config.system.build.toplevel;

          # Default package for `nix build .`
          default = nixosSystem.config.system.build.rootfsImage;
        }
      );

      # Expose the full NixOS configuration for inspection / extension.
      nixosConfigurations = builtins.listToAttrs (map (system: {
        name = "hive-vm-${system}";
        value = mkNixosSystem system;
      }) supportedSystems);
    };
}
