{
  description = "Elytra";

  inputs = {
    flake-parts.url = "github:hercules-ci/flake-parts";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = {...} @ inputs:
    inputs.flake-parts.lib.mkFlake {inherit inputs;} {
      systems = inputs.nixpkgs.lib.systems.flakeExposed;

      imports = [
        inputs.treefmt-nix.flakeModule
        inputs.flake-parts.flakeModules.easyOverlay
      ];

      perSystem = {
        system,
        config,
        ...
      }: let
        pkgs = import inputs.nixpkgs {inherit system;};
      in {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gofumpt
            golangci-lint
            gotools
            config.treefmt.build.wrapper
          ];
        };

        packages.elytra = pkgs.buildGoModule rec {
          pname = "elytra";
          version = inputs.self.rev or "dirty";
          src = inputs.self;
          vendorHash = "sha256-DX3HHTFV9hObfBdMfdBCoZAsyibS+BSRlbWzLgjYMbo=";
          ldflags = [
            "-s"
            "-w"
            "-X"
            "github.com/pyrohost/elytra/system.Version=${version}"
          ];
        };

        treefmt = {
          projectRootFile = "flake.nix";

          programs = {
            alejandra.enable = true;
            deadnix.enable = true;
            gofumpt = {
              enable = true;
              extra = true;
            };
            shellcheck.enable = true;
            shfmt = {
              enable = true;
              indent_size = 0; # 0 causes shfmt to use tabs
            };
            yamlfmt.enable = true;
          };
        };
      };
    };
}
