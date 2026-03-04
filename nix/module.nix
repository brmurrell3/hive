# NixOS module for Hive — control plane + multi-agent support.
#
# Minimal (control plane only):
#
#   services.hived = {
#     enable = true;
#     clusterRoot = "/home/deploy/hive-cluster";
#     openFirewall = true;
#   };
#
# With a local agent:
#
#   services.hived = {
#     enable = true;
#     clusterRoot = "/home/deploy/hive-cluster";
#     user = "deploy";
#     group = "users";
#     openFirewall = true;
#     agents.assistant = {
#       manifest = "/home/deploy/hive-cluster/agents/assistant/manifest.yaml";
#       runtimeCommand = "${userHome}/.local/bin/openclaw";
#       runtimeArgs = [ "gateway" ];
#       runtimeConfig = "/home/deploy/hive-cluster/agents/assistant/openclaw.json";
#       runtimeConfigDest = ".openclaw/openclaw.json";
#       joinTokenFile = "/home/deploy/hive-cluster/.secrets/join-token";
#       httpAddr = ":9100";
#     };
#   };

flake:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.hived;
  system = pkgs.stdenv.hostPlatform.system;
  defaultPackage = flake.packages.${system}.hived;
  hivectlPackage = flake.packages.${system}.hivectl;
  agentPackage = flake.packages.${system}.hive-agent;
  userHome = if cfg.user == "hive" then "/var/lib/hive" else "/home/${cfg.user}";
  hasAgents = cfg.agents != {};

  # Check if any agent uses OpenClaw.
  needsOpenClaw = lib.any (agentCfg: agentCfg.useOpenClaw) (lib.attrValues cfg.agents);

  agentSubmodule = lib.types.submodule {
    options = {
      manifest = lib.mkOption {
        type = lib.types.str;
        description = "Path to the agent manifest YAML file.";
      };

      controlPlane = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1:4222";
        description = "Control plane address (host:port).";
      };

      joinTokenFile = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Path to a file containing the join token. The file should be readable only by the service user (chmod 600).";
      };

      runtimeCommand = lib.mkOption {
        type = lib.types.str;
        default = "${userHome}/.local/bin/openclaw";
        description = "Full path to the runtime binary.";
      };

      runtimeArgs = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ "gateway" ];
        description = "Arguments to pass to the runtime command.";
      };

      runtimeConfig = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Path to runtime config file (e.g., openclaw.json). Copied to agent's isolated HOME.";
      };

      runtimeConfigDest = lib.mkOption {
        type = lib.types.str;
        default = ".openclaw/openclaw.json";
        description = "Destination path relative to agent HOME for the runtime config.";
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

      useOpenClaw = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Whether this agent uses OpenClaw as its runtime.";
      };
    };
  };
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

    envFile = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Path to an environment file (KEY=VALUE per line). Variables are substituted into agent runtime configs via envsubst, so openclaw.json can use \${OPENROUTER_API_KEY} etc. without duplicating secrets.";
    };

    agents = lib.mkOption {
      type = lib.types.attrsOf agentSubmodule;
      default = {};
      description = "Set of agents to run on this machine. Each key is the agent ID.";
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

    # All systemd services merged into a single attribute set.
    systemd.services = {
      # hived control plane service.
      hived = {
        description = "Hive Control Plane";
        after = [ "network.target" ];
        wantedBy = [ "multi-user.target" ];

        serviceConfig = {
          Type = "notify";
          User = cfg.user;
          Group = cfg.group;
          ExecStart = "${cfg.package}/bin/hived --cluster-root ${cfg.clusterRoot}";
          Restart = "on-failure";
          RestartSec = 5;
        };
      };

      # Shared OpenClaw install — oneshot that agent services depend on.
      hive-openclaw-install = lib.mkIf (hasAgents && needsOpenClaw) {
        description = "Install OpenClaw (shared)";
        after = [ "network.target" ];
        wantedBy = [ "multi-user.target" ];

        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          User = cfg.user;
          Group = cfg.group;
          ExecStart = pkgs.writeShellScript "hive-openclaw-install" ''
            export PATH="${lib.makeBinPath [ pkgs.nodejs_22 ]}:$PATH"
            export HOME="${userHome}"

            OPENCLAW_BIN="${userHome}/.local/bin/openclaw"
            if [ ! -x "$OPENCLAW_BIN" ]; then
              echo "Installing OpenClaw..."
              ${pkgs.nodejs_22}/bin/npm install -g --prefix "${userHome}/.local" --ignore-scripts openclaw@latest
            else
              echo "OpenClaw already installed."
            fi
          '';
        };
      };
    } // lib.mapAttrs' (name: agentCfg:
      let
        agentHome = "${userHome}/.hive-agents/${name}";
        workDir = if agentCfg.workDir != "" then agentCfg.workDir else "${userHome}/hive-workspace";
        isOpenClaw = agentCfg.useOpenClaw;
        runtimeArgsStr = lib.concatStringsSep " " agentCfg.runtimeArgs;

        setupScript = pkgs.writeShellScript "hive-agent-${name}-setup" ''
          export HOME="${agentHome}"

          # Create isolated HOME for this agent.
          mkdir -p "${agentHome}"

          # Source shared env file (for envsubst of runtime config).
          ${lib.optionalString (cfg.envFile != "") ''
            if [ -f "${cfg.envFile}" ]; then
              set -a
              . "${cfg.envFile}"
              set +a
            fi
          ''}

          # Copy runtime config to agent's isolated HOME.
          ${lib.optionalString (agentCfg.runtimeConfig != "") ''
            DEST_DIR=$(dirname "${agentHome}/${agentCfg.runtimeConfigDest}")
            mkdir -p "$DEST_DIR"
            cp "${agentCfg.runtimeConfig}" "${agentHome}/${agentCfg.runtimeConfigDest}"
            ${lib.optionalString (cfg.envFile != "") ''
              ${pkgs.gettext}/bin/envsubst < "${agentCfg.runtimeConfig}" > "${agentHome}/${agentCfg.runtimeConfigDest}"
            ''}
            chown ${cfg.user}:${cfg.group} "$DEST_DIR" "${agentHome}/${agentCfg.runtimeConfigDest}"
            chmod 600 "${agentHome}/${agentCfg.runtimeConfigDest}"
          ''}

          # Ensure work directory exists.
          mkdir -p "${workDir}"

          # Sync agent files from cluster root to workspace.
          for f in AGENTS.md SKILL.md MEMORY.md IDENTITY.md USER.md SOUL.md; do
            src="${cfg.clusterRoot}/agents/${name}/$f"
            if [ -f "$src" ]; then
              cp "$src" "${workDir}/$f"
            fi
          done

          # Wait for hived to write the NATS auth token.
          for i in $(seq 1 30); do
            [ -f "${cfg.clusterRoot}/.state/nats-auth-token" ] && break
            sleep 1
          done
        '';

        joinScript = pkgs.writeShellScript "hive-agent-${name}-join" ''
          NATS_TOKEN_FILE="${cfg.clusterRoot}/.state/nats-auth-token"
          NATS_TOKEN=""
          if [ -f "$NATS_TOKEN_FILE" ]; then
            NATS_TOKEN=$(cat "$NATS_TOKEN_FILE")
          fi

          JOIN_TOKEN=""
          ${lib.optionalString (agentCfg.joinTokenFile != "") ''
            if [ -f "${agentCfg.joinTokenFile}" ]; then
              JOIN_TOKEN=$(cat "${agentCfg.joinTokenFile}")
            fi
          ''}

          exec ${agentPackage}/bin/hive-agent join \
            --control-plane ${agentCfg.controlPlane} \
            --agent-id ${name} \
            --manifest ${agentCfg.manifest} \
            --runtime-cmd ${agentCfg.runtimeCommand} \
            ${lib.optionalString (runtimeArgsStr != "") "--runtime-args ${runtimeArgsStr}"} \
            --work-dir ${workDir} \
            --http-addr ${agentCfg.httpAddr} \
            ''${JOIN_TOKEN:+--token "$JOIN_TOKEN"} \
            ''${NATS_TOKEN:+--nats-token "$NATS_TOKEN"}
        '';

        # Build dependency list — only depend on openclaw-install if this agent uses openclaw.
        agentDeps = [ "hived.service" ]
          ++ lib.optionals isOpenClaw [ "hive-openclaw-install.service" ];
      in
      lib.nameValuePair "hive-agent-${name}" {
        description = "Hive Agent (${name})";
        after = [ "network.target" ] ++ agentDeps;
        requires = agentDeps;
        wantedBy = [ "multi-user.target" ];

        path = [ pkgs.git ] ++ lib.optionals isOpenClaw [ pkgs.nodejs_22 ];

        environment = {
          HOME = agentHome;
          PATH = lib.mkForce (
            "${lib.optionalString isOpenClaw "${userHome}/.local/bin:"}"
            + "${lib.makeBinPath ([ pkgs.git ] ++ lib.optionals isOpenClaw [ pkgs.nodejs_22 ])}"
            + ":/run/current-system/sw/bin"
          );
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
    ) cfg.agents;

    # System packages.
    environment.systemPackages = [
      hivectlPackage
    ] ++ lib.optionals needsOpenClaw [
      pkgs.nodejs_22
    ];

    # Open NATS port if requested.
    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ 4222 ];
  };
}
