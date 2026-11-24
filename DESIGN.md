# portmesh design

## What exists, and why we build our own

| Tool | Shape | Why it does not fit |
|---|---|---|
| frp (frps + frpc, stcp visitor) | Hub relay; frpc on the target exposes a port, an frpc "visitor" on another device binds any `bindAddr:bindPort` | Data path fits exactly. Config is a static TOML per device with no central editing; status is split between the frps dashboard (traffic) and each frpc's log (bind errors such as "172.17.0.1 not available"), so the UI we want would still be ours to write. |
| frp-panel (VaalaCat) | Central web UI over frp, clients connect outbound | The closest fit. But it is a large third-party stack (master + server + client roles, its own users and DB), no documented macOS service install, and per-forward bind errors on the visitor side are not surfaced. We would own the operational surface without owning the code. |
| rathole, chisel | Client/server tunnels | The server side is the only place that binds; no "device A to device B" and no UI. |
| boringproxy | Web UI tunnels | Built for public HTTPS on domains; TCP tunnels bind on the server only; dormant project. |
| Tailscale serve | Exposes a local port on the tailnet | Cannot bind an arbitrary address (docker0) on another machine, and every device must be on the tailnet. |
| sshuttle | VPN over ssh | Not port forwarding. |
| Controller driving `ssh -R`/`-L` | autossh per forward | Needs inbound ssh to one side of every forward, a key matrix, and gives no byte or session counts. |

The data path needed is small (one relay, a handful of forwards, LAN traffic), and the part that does not exist anywhere is the central config plus per-forward status from both ends. One Go binary, a few hundred lines of core logic, is less to operate than frp-panel, which remains the closest alternative: each portmesh forward maps onto one frp stcp proxy plus one visitor.

## Data path

Every agent holds one outbound TLS connection to the controller, multiplexed with yamux. No device ever accepts an inbound connection from another device.

```
container ─► 172.17.0.1:8080 (agent on the docker host, listening on the docker0 gateway)
   └─ yamux stream ─► controller (relay, counts bytes) ─ yamux stream ─► agent on a laptop
                                                                            └─ dial 127.0.0.1:8080
```

1. The source agent listens on `bindAddr:port`. On accept it opens a stream to the controller with a header `{forward_id}`.
2. The controller checks the forward exists and the stream came from its source device, opens a stream to the target device's session with `{forward_id, host, port}`, and splices the two, counting bytes.
3. The target agent checks the request matches a forward assigned to it as target, dials `host:port`, and splices.

Relaying through the controller costs one extra LAN hop; for this traffic that is irrelevant, and it is what makes the outbound-only rule hold.

## Control path

Stream 0 on each session carries JSON messages. The controller pushes the device's full desired state (forwards where it is the source or target) on connect and on every change. The agent reports per forward: listening / bind error (retried every 5 s, so a docker0 that is not up yet heals by itself), active sessions, last dial error. The controller adds bytes in/out and connection state, and the UI polls `/api/state` every 2 s.

Agents ping every 10 s and drop the session after 30 s of silence, then reconnect with backoff (1 s up to 30 s). Listeners stay open while the controller is unreachable and reset accepted connections, so a reconnect does not race docker for the bind.

Config is a JSON file in the controller's volume (devices, token hashes, forwards); it is the single source of truth.

## Auth

- The controller generates a self-signed TLS cert at first start. Agents pin its SHA-256 fingerprint, given in the install command.
- "Add device" in the UI mints a random token, shown once; the controller keeps only its hash. The agent presents it in its hello message. Unknown or revoked tokens are dropped, so the agent port is not an open relay: streams only flow for configured forwards between authenticated devices.
- Web UI: no login. Anyone who can open it can expose any loopback port of any device, so it belongs on a trusted network only, never on the internet.

## Install story

- One binary `portmesh` with `controller` and `agent` subcommands, cross-compiled for darwin/arm64, linux/amd64, linux/arm64. The controller serves the binaries and an `install.sh`.
- Controller: one container. Ports: `7900` agents (TLS), `7901` web UI.
- Agent: "Add device" shows a one-liner, `curl -fsSL http://<controller>:7901/install.sh | sudo sh -s -- <name> <token>`, with the controller address and fingerprint baked into the script. It installs the binary, `/etc/portmesh/agent.json`, and:
  - macOS: a LaunchDaemon `io.portmesh.agent` with `UserName` set to the user who ran sudo, and `KeepAlive`. A daemon starts at boot without a login and, per Apple TN3179, is exempt from Local Network privacy, which would block a LaunchAgent from reaching the controller on the LAN.
  - Linux: a systemd unit `portmesh-agent.service`, `Restart=always`, `After=network-online.target docker.service`, running as an unprivileged user (all binds are above 1024).
- The controller's own host runs an agent on the host, not in the controller container, when a forward must bind a host address such as docker0, because that address lives in the host's network namespace.

## Example: a container reaching a laptop's loopback service

A service on a laptop listens on `127.0.0.1:8080` only. A container on a docker host must reach it as `host.docker.internal:8080` (`extra_hosts: host.docker.internal:host-gateway`, which resolves to the docker0 gateway `172.17.0.1`).

1. Run the controller; add devices `docker-host` and `laptop`; run the install command on each.
2. On the map, drag from `laptop` to `docker-host`, enter port `8080`, and under Addresses set the listen address to `172.17.0.1`.
3. The `docker-host` agent binds `172.17.0.1:8080` (retrying every 5 s until docker0 exists), and the container's connections reach the laptop's `127.0.0.1:8080`. The UI shows the open sessions and bytes; a failed bind names the process holding the port.

## Not built yet

UDP forwards, direct device-to-device data path, multiple users, metrics history.
