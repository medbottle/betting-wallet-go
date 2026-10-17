# Runs the multiinstance suite with the race detector (README, "Multi-instance
# tests and failure simulations"; ARCHITECTURE §25). -race needs cgo, so the
# suite runs in the Debian Go image, which has gcc, plus the Docker CLI with
# its Compose and Buildx plugins to drive the stack through the mounted
# Docker socket. Build it without a context:
#
#   docker build -t wager-wallet-race-harness - < test/multiinstance/race.Dockerfile
FROM docker:29.8.2-cli AS cli

FROM golang:1.27.2-bookworm
COPY --from=cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=cli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/
