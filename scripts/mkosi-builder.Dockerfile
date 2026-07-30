# mkosi-builder — the image-build toolchain, pinned and self-contained.
#
# Everything a system-image build needs lives here rather than on the CI
# runner: the runner then needs only Docker, the toolchain cannot drift
# between runners, and the same build runs identically on a laptop.
#
# Debian is the build host regardless of what is being built — mkosi bootstraps
# the target distribution from its own archive, so an Ubuntu image builds fine
# from here (see ubuntu-keyring below).
FROM debian:trixie-slim

# The runner user is uid 1000; matching it keeps bind-mounted build output
# owned by the invoking user instead of root.
ARG BUILDER_UID=1000
ARG BUILDER_GID=1000

# systemd-repart is a separate package on Debian, not part of systemd, and
# uidmap is only a Recommends — so a --no-install-recommends image has neither
# unless they are named explicitly. Both are required: repart lays down the
# partition table offline, and uidmap provides the setuid newuidmap/newgidmap
# helpers that map mkosi's user namespace.
#
# ubuntu-keyring is the cross-distro requirement: apt on a Debian host refuses
# to verify the Ubuntu archive signature without it.
#
# qemu-utils and awscli are for the publish half — qcow2 conversion and the
# upload to R2 — so build and publish share one toolchain.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        mkosi \
        systemd-repart \
        uidmap \
        ubuntu-keyring \
        debian-archive-keyring \
        qemu-utils \
        awscli \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

# Profiles= is the basis of the composition model and landed in mkosi 24. An
# older toolchain parses the config and ignores the profile silently, yielding
# a base image with none of the composed content, so fail at image build time
# rather than shipping a quietly wrong artefact.
RUN set -eu; \
    v="$(mkosi --version | grep -oE '[0-9]+' | head -1)"; \
    if [ "${v}" -lt 24 ]; then \
        echo "mkosi ${v} too old — Profiles= requires >= 24" >&2; exit 1; \
    fi

# mkosi must not run as root here. As root it sees uid 0, concludes it is
# privileged, and unshares the mount namespace on its own — which needs real
# CAP_SYS_ADMIN that a non-privileged container does not have. As an ordinary
# user it takes the user-namespace path instead and gains CAP_SYS_ADMIN inside
# that namespace, which is what the sandbox is designed around.
#
# The subordinate id ranges are the ids that namespace maps onto. They are set
# here rather than on the host precisely so no runner has to carry them.
RUN groupadd -g "${BUILDER_GID}" builder \
    && useradd -m -u "${BUILDER_UID}" -g "${BUILDER_GID}" builder \
    && echo "builder:165536:65536" > /etc/subuid \
    && echo "builder:165536:65536" > /etc/subgid

# The cache path must exist in the image, owned by builder, before anything
# mounts a named volume over it: Docker seeds a fresh volume from the image's
# directory (ownership included), but creates it empty and root-owned if the
# path is absent — which mkosi then cannot write to.
RUN install -d -o builder -g builder -m 0755 /home/builder/.cache

USER builder
WORKDIR /work
