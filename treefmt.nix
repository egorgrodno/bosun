{
  projectRootFile = "flake.nix";

  programs.nixfmt.enable = true;
  programs.deadnix.enable = true;
  # After deadnix, to lay out what its edits leave.
  programs.nixfmt.priority = 1;
  programs.gofmt.enable = true;
  programs.rumdl-format.enable = true;

  settings.global.excludes = [
    "flake.lock"
    "go.mod"
    ".envrc"
    ".gitignore"
  ];
}
