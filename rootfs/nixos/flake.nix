{
  description = "Hive NixOS rootfs for Firecracker VMs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-24.11";
  };

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };

      # The NixOS system configuration, evaluated separately so we can
      # reference its build outputs (toplevel, kernel, ext4 image).
      nixosSystem = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          ./configuration.nix
        ];
      };
    in
    {
      packages.${system} = {
        # Full ext4 rootfs image ready for Firecracker.
        rootfs = nixosSystem.config.system.build.rootfsImage;

        # Kernel binary (vmlinux) suitable for Firecracker direct boot.
        # The vmlinux file is at: result/bzImage (or result/vmlinux depending on config).
        kernel = nixosSystem.config.boot.kernelPackages.kernel;

        # The complete NixOS system closure (useful for debugging).
        toplevel = nixosSystem.config.system.build.toplevel;
      };

      # `nix build` with no fragment produces the rootfs image.
      defaultPackage.${system} = self.packages.${system}.rootfs;

      # Expose the full NixOS configuration for inspection / extension.
      nixosConfigurations.hive-vm = nixosSystem;
    };
}
