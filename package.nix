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
  vendorHash = "sha256-EQjgSjvv1WOb0DffOq93qlu1/6DlXCVwc4zyoXrEctw=";
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${rev}"
  ];
  meta.mainProgram = "homelab-ha";
}
