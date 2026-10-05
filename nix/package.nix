{
  lib,
  stdenv,
  buildGoModule,
  installShellFiles,
  makeBinaryWrapper,
  procps,
  lsof,
  src,
  version,
  revision,
  dirty ? false,
}:

buildGoModule {
  pname = "sessionio";
  inherit version;
  src = lib.cleanSourceWith {
    inherit src;
    filter = path: type:
      lib.cleanSourceFilter path type
      && !(lib.elem (baseNameOf path) [ ".worktrees" "__pycache__" "result" ])
      && !(type == "regular" && baseNameOf path == "sessionio");
  };

  vendorHash = "sha256-HP5g19tp2mGqks7VQkykyH7rTec5NOFUIVutC6bPFaI=";
  subPackages = [ "cmd/sessionio" ];
  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X github.com/nikitatsym/agent-session-io/internal/buildinfo.version=${version}"
    "-X github.com/nikitatsym/agent-session-io/internal/buildinfo.commit=${revision}"
    "-X github.com/nikitatsym/agent-session-io/internal/buildinfo.dirty=${lib.boolToString dirty}"
    "-X github.com/nikitatsym/agent-session-io/internal/buildinfo.packageManager=nix"
  ];

  nativeBuildInputs = [ installShellFiles makeBinaryWrapper ];

  preBuild = ''
    export HOME="$TMPDIR"
  '';

  postInstall = ''
    installShellCompletion --cmd sessionio \
      --bash <("$out/bin/sessionio" completion bash) \
      --zsh <("$out/bin/sessionio" completion zsh) \
      --fish <("$out/bin/sessionio" completion fish)
    install -Dm644 LICENSE "$out/share/licenses/sessionio/LICENSE"
    wrapProgram "$out/bin/sessionio" \
      --prefix PATH : ${lib.makeBinPath ([ lsof ] ++ lib.optional stdenv.hostPlatform.isLinux procps)}${lib.optionalString stdenv.hostPlatform.isDarwin ":/bin"}
  '';

  meta = {
    description = "Harness-neutral access to local coding-agent sessions";
    homepage = "https://github.com/nikitatsym/agent-session-io";
    license = lib.licenses.mpl20;
    mainProgram = "sessionio";
    platforms = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
  };
}
