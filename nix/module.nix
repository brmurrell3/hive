# NixOS module for Hive — control plane + optional local agent.
#
# Minimal (control plane only):
#
#   services.hived = {
#     enable = true;
#     clusterRoot = "/home/deploy/hive-cluster";
#     openFirewall = true;
#   };
#
# Full (control plane + local agent with OpenClaw):
#
#   services.hived = {
#     enable = true;
#     clusterRoot = "/home/deploy/hive-cluster";
#     user = "deploy";
#     group = "users";
#     openFirewall = true;
#     agent = {
#       enable = true;
#       id = "assistant";
#       manifest = "/home/deploy/hive-cluster/agents/assistant/manifest.yaml";
#       openclawConfig = "/home/deploy/hive-cluster/agents/assistant/openclaw.json";
#       joinToken = "abc123...";
#     };
#   };

flake:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.hived;
  agentCfg = cfg.agent;
  system = pkgs.stdenv.hostPlatform.system;
  defaultPackage = flake.packages.${system}.hived;
  hivectlPackage = flake.packages.${system}.hivectl;
  agentPackage = flake.packages.${system}.hive-agent;
in
{
  options.services.hived = {
    enable = lib.mkEnableOption "Hive control plane (hived)";

    package = lib.mkOption {
      type = lib.types.package;
      default = defaultPackage;
      description = "The hived package to use.";
    };

    clusterRoot = lib.mkOption {
      type = lib.types.str;
      description = "Path to the cluster root directory containing cluster.yaml.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "hive";
      description = "User account under which hived runs.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "hive";
      description = "Group under which hived runs.";
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Whether to open the NATS port (4222/tcp) in the firewall.";
    };

    agent = {
      enable = lib.mkEnableOption "local Hive agent on this machine";

      id = lib.mkOption {
        type = lib.types.str;
        default = "assistant";
        description = "Agent ID to register with the control plane.";
      };

      manifest = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Path to the agent manifest YAML file.";
      };

      controlPlane = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1:4222";
        description = "Control plane address (host:port).";
      };

      joinToken = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Join token for cluster authentication.";
      };

      openclawConfig = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Path to openclaw.json. Copied to ~/.openclaw/ for the agent user.";
      };

      workDir = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Working directory for the agent runtime. Defaults to ~/hive-workspace.";
      };

      httpAddr = lib.mkOption {
        type = lib.types.str;
        default = ":9100";
        description = "HTTP API listen address for the agent sidecar.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    # Create system user and group (only when using the default "hive" user).
    users.users.${cfg.user} = lib.mkIf (cfg.user == "hive") {
      isSystemUser = true;
      group = cfg.group;
      home = "/var/lib/hive";
      createHome = true;
      description = "Hive control plane service user";
    };

    users.groups.${cfg.group} = lib.mkIf (cfg.group == "hive") { };

    # hived systemd service.
    systemd.services.hived = {
      description = "Hive Control Plane";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];

      environment.HIVE_TEST_FIRECRACKER = "mock";

      serviceConfig = {
        Type = "simple";
        User = cfg.user;
        Group = cfg.group;
        ExecStart = "${cfg.package}/bin/hived --cluster-root ${cfg.clusterRoot}";
        Restart = "on-failure";
        RestartSec = 5;
      };
    };

    # hive-agent systemd service.
    systemd.services.hive-agent = lib.mkIf agentCfg.enable (
      let
        userHome = if cfg.user == "hive" then "/var/lib/hive" else "/home/${cfg.user}";
        workDir = if agentCfg.workDir != "" then agentCfg.workDir else "${userHome}/hive-workspace";

        # Script to install openclaw if missing and set up config.
        setupScript = pkgs.writeShellScript "hive-agent-setup" ''
          export PATH="${lib.makeBinPath [ pkgs.nodejs_22 ]}:$PATH"
          export HOME="${userHome}"

          # Install openclaw if not present.
          OPENCLAW_BIN="${userHome}/.local/bin/openclaw"
          if [ ! -x "$OPENCLAW_BIN" ]; then
            echo "Installing OpenClaw..."
            ${pkgs.nodejs_22}/bin/npm install -g --prefix "${userHome}/.local" --ignore-scripts openclaw@latest
          fi

          # Copy openclaw config if specified.
          ${lib.optionalString (agentCfg.openclawConfig != "") ''
            mkdir -p "${userHome}/.openclaw"
            cp "${agentCfg.openclawConfig}" "${userHome}/.openclaw/openclaw.json"
            chown ${cfg.user}:${cfg.group} "${userHome}/.openclaw/openclaw.json"
            chmod 600 "${userHome}/.openclaw/openclaw.json"
          ''}

          # Ensure work directory exists.
          mkdir -p "${workDir}"

          # Wait for hived to write the NATS auth token.
          for i in $(seq 1 30); do
            [ -f "${cfg.clusterRoot}/.state/nats-auth-token" ] && break
            sleep 1
          done
        '';

        # Wrapper script that reads the NATS auth token at runtime.
        joinScript = pkgs.writeShellScript "hive-agent-join" ''
          NATS_TOKEN_FILE="${cfg.clusterRoot}/.state/nats-auth-token"
          NATS_TOKEN=""
          if [ -f "$NATS_TOKEN_FILE" ]; then
            NATS_TOKEN=$(cat "$NATS_TOKEN_FILE")
          fi

          exec ${agentPackage}/bin/hive-agent join \
            --control-plane ${agentCfg.controlPlane} \
            --agent-id ${agentCfg.id} \
            --manifest ${agentCfg.manifest} \
            --runtime-cmd ${userHome}/.local/bin/openclaw \
            --runtime-args gateway \
            --work-dir ${workDir} \
            --http-addr ${agentCfg.httpAddr} \
            ${lib.optionalString (agentCfg.joinToken != "") "--token ${agentCfg.joinToken}"} \
            ''${NATS_TOKEN:+--nats-token "$NATS_TOKEN"}
        '';
      in
      {
        description = "Hive Agent (${agentCfg.id})";
        after = [ "network.target" "hived.service" ];
        requires = [ "hived.service" ];
        wantedBy = [ "multi-user.target" ];

        path = [ pkgs.nodejs_22 pkgs.git ];

        environment = {
          HOME = userHome;
          PATH = lib.mkForce "${userHome}/.local/bin:${lib.makeBinPath [ pkgs.nodejs_22 pkgs.git ]}:/run/current-system/sw/bin";
        };

        serviceConfig = {
          Type = "simple";
          User = cfg.user;
          Group = cfg.group;
          ExecStartPre = "+${setupScript}";
          ExecStart = joinScript;
          Restart = "on-failure";
          RestartSec = 10;
        };
      }
    );

    # System packages.
    environment.systemPackages = [
      hivectlPackage
    ] ++ lib.optionals agentCfg.enable [
      pkgs.nodejs_22
    ];

    # Open NATS port if requested.
    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ 4222 ];
  };
}
