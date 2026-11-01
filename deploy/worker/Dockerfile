# syntax=docker/dockerfile:1
FROM node:lts-trixie-slim
ARG CODEX_VERSION=0.166.0
ARG GH_VERSION=2.103.3
RUN --mount=type=secret,id=proxy_ca \
    apt-get update && apt-get install -y --no-install-recommends git ca-certificates openssh-client curl && \
    if [ -f /run/secrets/proxy_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/proxy_ca; fi && \
    npm install --global "@openai/codex@${CODEX_VERSION}" && \
    if [ -f /run/secrets/proxy_ca ]; then export CURL_CA_BUNDLE=/run/secrets/proxy_ca; fi && \
    cd /tmp && arch="$(dpkg --print-architecture)" && archive="gh_${GH_VERSION}_linux_${arch}.tar.gz" && \
    curl --fail --location --silent --show-error -O "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${archive}" && \
    curl --fail --location --silent --show-error -o checksums.txt "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_checksums.txt" && \
    awk -v filename="$archive" '$2 == filename' checksums.txt | sha256sum --check --strict - && \
    tar -xzf "$archive" --strip-components=2 -C /usr/local/bin "gh_${GH_VERSION}_linux_${arch}/bin/gh" && \
    rm -f "$archive" checksums.txt && \
    rm -rf /var/lib/apt/lists/* /root/.npm && \
    mkdir -p /workspace /output && chown node:node /workspace /output
USER node
WORKDIR /workspace
