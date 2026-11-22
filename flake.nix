{
  description = "A Home Assistant MQTT bridge for a homelab-ngx host, as a stack";

  # Only the package and dev shell below use it: the homelab daemon builds the
  # stack with the nixpkgs chosen for it (homelab-stack.json recommends one),
  # and makes this input follow that.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    let
      rev = self.shortRev or self.dirtyShortRev or "dirty";
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
    in
    {
      # What the daemon builds, on its base module (manifest version 8).
      nixosModules.default =
        { pkgs, ... }:
        {
          imports = [ ./stack.nix ];
          _module.args.bridge = pkgs.callPackage ./package.nix { inherit rev; };
        };

      packages.x86_64-linux = rec {
        homelab-ha = pkgs.callPackage ./package.nix { inherit rev; };
        default = homelab-ha;
      };

      devShells.x86_64-linux.default = pkgs.mkShell {
        packages = with pkgs; [
          go
          gopls
        ];
      };
    };
}
