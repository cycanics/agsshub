# agsshub dev hub: agsshub-hubd (its own SSH server) on port 2422, installed by install.sh. Build the
# daemon first (go/build.sh); test/dev-hub.sh does. For local integration tests only; its state lives in the
# container. See docs/dev-hub.md.
FROM ubuntu:26.04

ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-client procps tini \
 && rm -rf /var/lib/apt/lists/*

COPY . /src/agsshub
RUN test -x /src/agsshub/go/bin/agsshub-hubd || { echo "no go/bin/agsshub-hubd: run go/build.sh first" >&2; exit 1; } \
 && /src/agsshub/install.sh \
 && install -m 0755 /src/agsshub/dev-hub/entrypoint.sh /usr/local/sbin/dev-hub-entrypoint \
 && rm -f /etc/hub/ssh_host_* /etc/ssh/ssh_host_*

EXPOSE 2422
HEALTHCHECK --interval=2s --timeout=2s --start-period=20s --retries=10 \
  CMD bash -c 'exec 3<>/dev/tcp/127.0.0.1/2422' || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/sbin/dev-hub-entrypoint"]
