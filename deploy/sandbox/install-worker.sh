#!/bin/sh
set -eu

apt-get update
apt-get install -y --no-install-recommends bash coreutils findutils tar git ca-certificates openssh-client curl
npm install --global --no-audit --no-fund @openai/codex@0.166.0

gh_version=2.103.3
gh_arch="$(dpkg --print-architecture)"
gh_archive="gh_${gh_version}_linux_${gh_arch}.tar.gz"
gh_directory="$(mktemp -d)"
trap 'rm -rf "$gh_directory"' EXIT
cd "$gh_directory"
curl --fail --location --silent --show-error -O "https://github.com/cli/cli/releases/download/v${gh_version}/${gh_archive}"
curl --fail --location --silent --show-error -o checksums.txt "https://github.com/cli/cli/releases/download/v${gh_version}/gh_${gh_version}_checksums.txt"
awk -v filename="$gh_archive" '$2 == filename' checksums.txt | sha256sum --check --strict -
tar -xzf "$gh_archive" --strip-components=2 -C /usr/local/bin "gh_${gh_version}_linux_${gh_arch}/bin/gh"

if ! id user >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash user
fi
mkdir -p /workspace /output
chown user:user /workspace /output
chmod 0755 /workspace /output
rm -rf /var/lib/apt/lists/* /root/.npm
node --version
codex --version
git --version
gh --version
