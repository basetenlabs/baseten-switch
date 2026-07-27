# Fresh-install simulation image: a minimal stand-in for a brand-new
# Linux machine. Debian slim plus curl and CA certificates, one
# non-root user with a home directory, nothing else. The single
# baseten-switch binary is staged in /opt/dist (NOT on PATH);
# install-inside.sh places it on PATH, standing in for the go-build step that
# run.sh performs on the host.
#
# No secrets are baked into any layer: the Baseten API key arrives only
# as a docker run environment variable at runtime.

FROM debian:stable-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --shell /bin/bash release-user

COPY build/baseten-switch /opt/dist/
COPY install-inside.sh /opt/fresh-install/install-inside.sh

USER release-user
WORKDIR /home/release-user

ENTRYPOINT ["bash", "/opt/fresh-install/install-inside.sh"]
