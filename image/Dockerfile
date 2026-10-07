# syntax=docker/dockerfile:1
# ferrous/rustserver — Rust dedicated server, dockerized port of the proven
# local setup (steamcmd install/update + uMod Oxide match + InvDump plugin).
#
# The image is intentionally small: steamcmd + helpers. Game files (~4 GB)
# install into SERVER_DIR (/server) at first boot — the bind-mounted data dir
# the panel's Files tab edits. See docs/api-v1.md for the env/port contract.
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      jq \
      unzip \
      lib32gcc-s1 \
      lib32stdc++6 \
    && rm -rf /var/lib/apt/lists/*

# steamcmd (anonymous login downloads app 258550 at first boot)
RUN mkdir -p /opt/steamcmd \
    && curl -fsSL https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz \
       | tar -xz -C /opt/steamcmd

COPY update.sh oxide-install.sh InvDump.cs /opt/ferrous/
COPY entrypoint.sh /usr/local/bin/ferrous-entry
RUN chmod +x /opt/ferrous/update.sh /opt/ferrous/oxide-install.sh /usr/local/bin/ferrous-entry

ENV SERVER_DIR=/server
WORKDIR /server

# game (28015/udp) + query (28016/udp) get published by `POST /servers`;
# rcon (28016/tcp) is deliberately NOT exposed — the agent dials the
# container IP directly via the ferrous.rcon_port label.
EXPOSE 28015/udp 28016/udp

# proven systemd unit: KillSignal=SIGINT, TimeoutStopSec=180
STOPSIGNAL SIGINT

# exec'd by entrypoint: RustDedicated becomes PID 1 and handles the stop
# signal itself — no wrapper in between to drop it.
ENTRYPOINT ["/usr/local/bin/ferrous-entry"]
