{
  description = "A master Claude Code session that runs a tree of worker sessions";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    treefmt-nix.url = "github:numtide/treefmt-nix";
    treefmt-nix.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
    }:
    let
      forAllSystems =
        f: nixpkgs.lib.genAttrs nixpkgs.lib.systems.flakeExposed (s: f nixpkgs.legacyPackages.${s});
      treefmtEval = pkgs: treefmt-nix.lib.evalModule pkgs ./treefmt.nix;
    in
    {
      formatter = forAllSystems (pkgs: (treefmtEval pkgs).config.build.wrapper);

      checks = forAllSystems (pkgs: {
        formatting = (treefmtEval pkgs).config.build.check self;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
          ];
        };
      });
    };
}
