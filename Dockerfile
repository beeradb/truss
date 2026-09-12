# The truss image: the binary, plus exactly the tools it shells out to.
#
# ⚠️ IT IS BUILT AND PUSHED BY CI, NOT BY HAND ON A BOX. The applier's image
# used to be built with podman on the one machine that runs it, which made the
# image the single thing in the system not delivered by merging -- so a config
# change could outrun its runtime and only an SSH session could close the gap.
# On 2026-09-08 that took a consumer's applier down for hours, and the fix for
# it could not be shipped BY the applier. See .github/workflows/release.yml.
#
# ⚠️ WHAT IS IN HERE IS DECIDED BY WHAT TRUSS EXECUTES, and nothing else.
# Verified by grepping every exec.Command in non-test code: `tofu` (plan and
# apply), `git` (clone and read the approved commit), `op` (the publisher's
# 1Password reads), `kustomize` (rendering a delivery unit's manifests so they
# can be fingerprinted; internal/render.Runner.Build), `ansible-playbook`
# (configuring a managed machine; internal/ansible.Runner). Truss talks to GitHub
# and to S3 over HTTP in Go, so it needs no `gh` and no `aws` -- both of which
# the hand-built image carried.
#
# ⚠️ NO PROVIDER MIRROR. The old image baked one, built from the CONSUMER's
# terraform config, which is what coupled this image to somebody else's repo
# and made "add a provider" mean "rebuild the applier". Providers belong in a
# plugin cache the consumer owns.
ARG BASE_IMAGE=debian:trixie-slim
FROM ${BASE_IMAGE}

# Set by buildx, one value per --platform. Everything below that differs by
# architecture reads it, so a multi-arch build needs no per-arch Dockerfile
# and no `uname` guessing at runtime.
ARG TARGETARCH

# ⚠️ NO DEFAULT, AND A LITERAL HERE WOULD BE WRONG RATHER THAN MERELY UNTIDY.
# The consumer's CI plans with one OpenTofu version and the applier re-plans
# with this one; if they differ, every plan digest mismatches and every apply
# is refused. The version is part of the image TAG for exactly that reason, so
# a consumer pins the image whose tofu matches the version it plans with.
ARG OPENTOFU_VERSION
RUN test -n "$OPENTOFU_VERSION" || { echo "OPENTOFU_VERSION build-arg is required" >&2; exit 1; }

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl unzip gnupg git \
 && rm -rf /var/lib/apt/lists/*

# OpenTofu, from the release zip rather than an apt repo: one binary, one
# checksum, no extra archive to trust for a single file.
RUN set -eux; \
    curl -fsSL -o /tmp/tofu.zip \
      "https://github.com/opentofu/opentofu/releases/download/v${OPENTOFU_VERSION}/tofu_${OPENTOFU_VERSION}_linux_${TARGETARCH}.zip"; \
    curl -fsSL -o /tmp/tofu.sums \
      "https://github.com/opentofu/opentofu/releases/download/v${OPENTOFU_VERSION}/tofu_${OPENTOFU_VERSION}_SHA256SUMS"; \
    grep " tofu_${OPENTOFU_VERSION}_linux_${TARGETARCH}.zip\$" /tmp/tofu.sums \
      | sed "s|tofu_${OPENTOFU_VERSION}_linux_${TARGETARCH}.zip|/tmp/tofu.zip|" \
      | sha256sum -c -; \
    unzip -q /tmp/tofu.zip -d /usr/local/bin tofu; \
    rm -f /tmp/tofu.zip /tmp/tofu.sums; \
    chmod 0755 /usr/local/bin/tofu

# ⚠️ ONE KUSTOMIZE VERSION, NOT A LIST LIKE tofu-versions. release.yml builds
# one image per OpenTofu version because a consumer's CI plans with one tofu
# and the applier must re-plan with the matching one (see above); the
# renderer has no equivalent per-consumer coupling, so kustomize-version at
# the repo root holds exactly one version rather than a table.
#
# NO DEFAULT, for the same reason as OPENTOFU_VERSION: a literal here would
# make the pin silently stale instead of missing.
ARG KUSTOMIZE_VERSION
RUN test -n "$KUSTOMIZE_VERSION" || { echo "KUSTOMIZE_VERSION build-arg is required" >&2; exit 1; }

# kustomize, from the release tarball rather than an apt repo: one binary,
# one checksum, no extra archive to trust for a single file. Used to render
# delivery units so their manifests can be fingerprinted before being
# diffed and applied (internal/render.Runner.Build).
RUN set -eux; \
    curl -fsSL -o /tmp/kustomize.tar.gz \
      "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    curl -fsSL -o /tmp/kustomize.sums \
      "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/checksums.txt"; \
    grep " kustomize_v${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz\$" /tmp/kustomize.sums \
      | sed "s|kustomize_v${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz|/tmp/kustomize.tar.gz|" \
      | sha256sum -c -; \
    tar -xzf /tmp/kustomize.tar.gz -C /usr/local/bin kustomize; \
    rm -f /tmp/kustomize.tar.gz /tmp/kustomize.sums; \
    chmod 0755 /usr/local/bin/kustomize

# ⚠️ ONE ANSIBLE VERSION, AND THE PIN IS WEAKER THAN THE TWO ABOVE IT --
# SAID PLAINLY RATHER THAN IMPLIED BY THE SHAPE. tofu and kustomize are
# checksum-verified because a digest gate has TWO SIDES that must agree about
# the tool: the consumer's CI plans or renders with one, the applier re-does
# it with this one, and a mismatch refuses every apply. A play has no second
# side (internal/ansible's package doc: CI cannot reach the hosts, so nothing
# CI could file about a play is a check that can fail), so what a pin buys
# here is reproducibility of the image, not agreement between two parties.
#
# What that costs, exactly: pip resolves ansible-core's transitive
# dependencies -- resolvelib, PyYAML, Jinja2, cryptography -- at build time
# and this does not hash-pin them. Closing that means a --require-hashes
# requirements file per architecture, because cryptography ships arch-
# specific wheels. Worth doing; not done, and not pretended.
ARG ANSIBLE_VERSION
RUN test -n "$ANSIBLE_VERSION" || { echo "ANSIBLE_VERSION build-arg is required" >&2; exit 1; }

# ⚠️ openssh-client IS NOT OPTIONAL AND IS EASY TO FORGET. Ansible's default
# connection plugin does not speak SSH itself -- it execs the local `ssh`
# binary -- so without this every play fails at the first host with a
# connection error naming no cause anyone can act on. The applier reaches a
# managed machine over Tailscale SSH and carries no private key of its own
# (cmd/truss/ansible_unit.go, ansibleEnv), but it still needs the client.
#
# ansible-core, not the `ansible` metapackage: the metapackage bundles
# roughly a hundred collections nobody here has read, and this image's
# standing rule is that what is in it is decided by what truss executes.
# Collections come from ansible-collections beside this file, one per line,
# each pinned -- an unpinned collection is a third party changing what runs
# as root on somebody else's machine, between two builds of the same commit.
COPY ansible-collections /tmp/ansible-collections
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      python3 python3-venv openssh-client; \
    python3 -m venv /opt/ansible; \
    /opt/ansible/bin/pip install --no-cache-dir --upgrade pip; \
    /opt/ansible/bin/pip install --no-cache-dir "ansible-core==${ANSIBLE_VERSION}"; \
    ln -s /opt/ansible/bin/ansible-playbook /usr/local/bin/ansible-playbook; \
    while IFS= read -r line; do \
      case "$line" in ''|\#*) continue ;; esac; \
      ANSIBLE_COLLECTIONS_PATH=/opt/ansible/collections \
        /opt/ansible/bin/ansible-galaxy collection install "$line"; \
    done < /tmp/ansible-collections; \
    rm -f /tmp/ansible-collections; \
    rm -rf /var/lib/apt/lists/*

# ⚠️ THE COLLECTIONS LIVE OUTSIDE THE DEFAULT SEARCH PATH, so this variable
# is what makes them findable. Without it ansible looks in ~/.ansible and
# /usr/share/ansible, finds neither, and reports the play's modules as
# missing -- which reads as a broken play rather than a broken image.
# ⚠️ READ BY truss, NOT BY ansible-playbook, AND THE DIFFERENCE IS NOT
# COSMETIC. This sets the variable in the TRUSS process's environment;
# cmd/truss/ansibleCollectionsPath reads it from there and passes it on to
# the play by name. It does not reach ansible-playbook on its own, because
# ansible.Runner builds the child's environment explicitly and inherits
# nothing -- a property that exists so a play running as root on somebody
# else's machine does not receive every credential this process holds.
#
# For a year this line looked like it configured ansible and configured
# nothing: a pass failed with "couldn't resolve module/action
# 'ansible.posix.mount'" while that collection was pinned below and
# installed right here. Deleting this line still breaks collections; so
# does deleting the passthrough in cmd/truss. Both halves are required.
ENV ANSIBLE_COLLECTIONS_PATH=/opt/ansible/collections

# The 1Password CLI, used only by the publisher. Its apt repo is per-arch, so
# the component below is TARGETARCH rather than a hardcoded amd64 -- which is
# the bug that made the hand-built image unbuildable on an arm64 laptop.
RUN set -eux; \
    curl -fsSL https://downloads.1password.com/linux/keys/1password.asc \
      | gpg --dearmor -o /usr/share/keyrings/1password-archive-keyring.gpg; \
    echo "deb [arch=${TARGETARCH} signed-by=/usr/share/keyrings/1password-archive-keyring.gpg] https://downloads.1password.com/linux/debian/${TARGETARCH} stable main" \
      > /etc/apt/sources.list.d/1password.list; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends 1password-cli; \
    rm -rf /var/lib/apt/lists/*

# ⚠️ THE BINARY IS BUILT OUTSIDE THIS FILE, still deliberately. Truss has zero
# dependencies, so `CGO_ENABLED=0 go build` produces the identical static
# artefact anywhere -- release.yml builds one per architecture and proves it
# reproduces before publishing. A builder stage here would download the module
# inside every image build for no benefit.
COPY dist/truss-linux-${TARGETARCH} /usr/local/bin/truss
RUN chmod 0755 /usr/local/bin/truss

# 10001 matches the `applier` user the platform image used, so a consumer's
# volume ownership does not change under them.
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin applier
WORKDIR /work
RUN chown 10001:10001 /work
USER 10001

# No default subcommand: `truss` with no arguments prints usage and exits 2,
# so a manifest that forgets its argument (`apply`, `loop`, `publish`, ...)
# fails loudly rather than doing something plausible.
ENTRYPOINT ["/usr/local/bin/truss"]
