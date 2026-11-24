{ lib, buildGoModule, rev }:
buildGoModule {
  pname = "homelab-ha";
  version = "0-${rev}";
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./go.mod
      ./go.sum
      ./cmd
      ./internal
    ];
  };
  vendorHash = "sha256-3YwL6s3ws3CsJBZJxvpHbjhvu9GK1GefcvfYsRIRCqQ=";
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${rev}"
  ];
  meta.mainProgram = "homelab-ha";
}
