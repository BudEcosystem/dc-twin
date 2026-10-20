# The image every NVSentinel container in compat/nvsentinel runs from (lib/build.sh): Debian with
# libsystemd0 (go-systemd's sdjournal loads it to read journal files), journalctl,
# systemd-journal-remote (it writes the journal file the twin's export stream describes), and
# NVSentinel's binaries, built from source at the pinned tag, in /nvsentinel/bin.
ARG BASE=docker.io/library/debian:bookworm-slim
FROM ${BASE}
RUN apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      libsystemd0 systemd systemd-journal-remote ca-certificates util-linux coreutils procps \
 && rm -rf /var/lib/apt/lists/*
COPY bin/ /nvsentinel/bin/
ENV PATH=/nvsentinel/bin:$PATH
