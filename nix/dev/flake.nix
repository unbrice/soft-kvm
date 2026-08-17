# SPDX-FileCopyrightText: 2026 Brice Arnould
#
# SPDX-License-Identifier: MIT OR Apache-2.0

{
  description = "soft-kvm dev shell and build tooling (contributor flake)";

  inputs = {
    nixpkgs.url = "nixpkgs";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = nixpkgs.lib.genAttrs systems;
      pkgsFor = system: import nixpkgs {
        inherit system;
      };

      mkPkg = system:
        let
          pkgs = pkgsFor system;
          soft-kvm = pkgs.buildGoModule {
            pname = "soft-kvm";
            version = "0.1.0";
            src = pkgs.lib.cleanSource ./../..;
            vendorHash = null;
            CGO_ENABLED = 0;
            meta = {
              description = "Display-Follows-Keyboard coordination daemon and client";
              mainProgram = "soft-kvm";
            };
          };
        in
        {
          inherit pkgs soft-kvm;
        };
    in
    {
      packages = forAll (system:
        let p = mkPkg system; in {
          default = p.soft-kvm;
        });

      apps = forAll (system: {
        default = {
          type = "app";
          program = "${(mkPkg system).soft-kvm}/bin/soft-kvm";
        };
      });

      devShells = forAll (system:
        let
          pkgs = pkgsFor system;
        in
        {
          ci = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              golangci-lint
              just
              reuse
              dprint
            ];
            CGO_ENABLED = "0";
          };

          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              gopls
              golangci-lint
              just
              dprint
              reuse
            ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
              ddcutil
            ];

            shellHook = ''
              export CGO_ENABLED=0
              # Automatically configure git hooks
              just setup-hooks 2>/dev/null || true
            '';
          };
        });

      formatter = forAll (system: (pkgsFor system).nixpkgs-fmt);
    };
}
