# The Fence Agents Remediation manager and the ClusterLabs fence agents in one image, as FAR's own
# image ships them (its Dockerfile: the manager binary on CentOS Stream 9 with the fence-agents
# RPMs): FAR runs the agent as a child process of the manager. Here the agents are built from
# the ClusterLabs source at a pinned tag (autotools, only the two agents this harness uses), and
# the manager is FAR built from its tag for linux by run.sh. Built by compat/medik8s/run.sh; the
# build context holds `fence-agents/` (the source tree, with .tarball-version) and `manager`.
FROM docker.io/library/debian:bookworm-slim

COPY fence-agents /src/fence-agents
# One layer: the agents' runtime (python3 with pexpect, pycurl, requests; ipmitool) stays, the
# build tools are removed once the agents are installed.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       python3 python3-pexpect python3-pycurl python3-requests ipmitool ca-certificates \
       autoconf automake libtool make gcc libc6-dev pkg-config xsltproc libxml2-utils \
    && cd /src/fence-agents \
    && ./autogen.sh >/dev/null \
    && ./configure --prefix=/usr --with-agents="ipmilan redfish" >/dev/null \
    && make >/dev/null \
    && make install >/dev/null \
    && fence_ipmilan --version && fence_redfish --version \
    && cd / && rm -rf /src \
    && apt-get purge -y --auto-remove autoconf automake libtool make gcc libc6-dev pkg-config xsltproc libxml2-utils \
    && rm -rf /var/lib/apt/lists/*

COPY manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
