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
            golangci-lint
            gotools
            config.treefmt.build.wrapper
          ];
        };

        packages.elytra = pkgs.buildGoModule rec {
          pname = "elytra";
          version = inputs.self.rev or "dirty";
          src = inputs.self;
          vendorHash = "sha256-scQQP9hRezE0pB6zh36IEqETktWwE8PH4gBCel8L3hQ=";
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
            gofmt.enable = true;
            shellcheck.enable = true;
            shfmt = {
              enable = true;
              indent_size = 0; # 0 causes shfmt to use tabs
            };
            yamlfmt = {
              enable = true;
              settings = {
                formatter.retain_line_breaks = true;
              };
            };
          };
        };
      };
    };
}
