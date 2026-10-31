ARG BASE_IMAGE=node:24-alpine
FROM ${BASE_IMAGE}
USER root
RUN mkdir -p /workspace /output && chown node:node /workspace /output
COPY --chmod=0755 codex.mjs /usr/local/bin/codex
USER node
WORKDIR /workspace
