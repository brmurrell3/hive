{
  description = "Hive - AI agent orchestration framework";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # NixOS module (works on any architecture)
      nixosModule = import ./nix/module.nix self;
    in
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        version = self.shortRev or self.dirtyShortRev or "dev";

        buildHiveBin = name: pkgs.buildGoModule {
          pname = name;
          inherit version;
          src = self;

          vendorHash = "sha256-M1nl7m22tOyVtta/Y323/fxbv2ZLoc4kPKr4PBUxMcE=";

          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" "-X main.version=${version}" ];

          subPackages = [ "cmd/${name}" ];

          meta = {
            description = "Hive ${name}";
            license = pkgs.lib.licenses.mit;
            mainProgram = name;
          };
        };
      in
      {
        packages = {
          hived = buildHiveBin "hived";
          hivectl = buildHiveBin "hivectl";
          hive-agent = buildHiveBin "hive-agent";
          default = buildHiveBin "hived";
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
            go-tools
            gnumake
          ];
        };
      }
    ) // {
      nixosModules.default = nixosModule;
    };
}
