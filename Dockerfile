# The truss image: one static binary, plus the tools it shells out to.
#
# ⚠️ BASE_IMAGE HAS NO DEFAULT, ON PURPOSE. Truss needs `tofu` and `git` on
# PATH and a CA bundle; it needs nothing else. Naming a base here would put a
# deployment's own image name in a repository written to be public, and
# scripts/leakscan refuses exactly that. The caller supplies it.
#
# ⚠️ AND THE BASE SHOULD BE THE IMAGE THE DEPLOYMENT ALREADY RUNS. `tofu` and
# `git` are the two things truss's behaviour is decided by, and both come from
# the base. Build on a different base and a change in what an apply does could
# be truss or could be a different tofu build, with nothing on hand to say
# which.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

# ⚠️ THE BINARY IS BUILT OUTSIDE THIS FILE, and that is deliberate rather than
# lazy. A builder stage would need the module downloaded inside the image
# build; truss has ZERO dependencies, so `CGO_ENABLED=0 GOOS=linux
# GOARCH=amd64 go build` on any machine produces the identical static
# artefact, and the build host does not have to match the cluster's
# architecture. See scripts/build-image.
ARG TRUSS_BINARY=truss
COPY ${TRUSS_BINARY} /usr/local/bin/truss

USER 0
RUN chmod 0755 /usr/local/bin/truss
# Back to the base's non-root user. 10001 matches the applier image's own
# `applier` user, which owns /work.
USER 10001

# No default subcommand: `truss` with no arguments prints usage and exits 2,
# so a CronJob that forgets its argument fails loudly instead of doing
# something plausible.
ENTRYPOINT ["/usr/local/bin/truss"]
