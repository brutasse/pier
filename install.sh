#!/bin/sh
# Install the latest pier release (Linux x86_64/arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/brutasse/pier/v0.1.0/install.sh | sh
#
# The tag in the URL is the release that ships this script; the script
# installs the latest release binary, verified against sha256sums.
# Pin a specific version:
#
#   PIER_VERSION=v0.1.0 curl -fsSL https://raw.githubusercontent.com/brutasse/pier/v0.1.0/install.sh | sh

set -eu

repo="brutasse/pier"
version="${PIER_VERSION:-}"

if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | head -n 1)
fi
if [ -z "$version" ]; then
  echo "error: could not determine the latest release version" >&2
  exit 1
fi

os=$(uname -s)
if [ "$os" != "Linux" ]; then
  echo "error: unsupported platform ${os} (Linux only; use the docker image ghcr.io/${repo})" >&2
  exit 1
fi
arch=$(uname -m)
case "$arch" in
x86_64)
  arch=amd64
  ;;
aarch64 | arm64)
  arch=arm64
  ;;
*)
  echo "error: unsupported architecture ${arch} (need x86_64 or arm64)" >&2
  exit 1
  ;;
esac

name="pier_${version}_linux_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "installing pier ${version} (${os}/${arch})"
curl -fsSL -o "${tmp}/${name}" "https://github.com/${repo}/releases/download/${version}/${name}"
curl -fsSL -o "${tmp}/sha256sums" "https://github.com/${repo}/releases/download/${version}/sha256sums"
(
  cd "$tmp"
  grep " ${name}\$" sha256sums | sha256sum -c -
)
tar -xzf "${tmp}/${name}" -C "$tmp"

if [ "$(id -u)" = "0" ] || [ -w /usr/local/bin ]; then
  dest_dir=/usr/local/bin
else
  dest_dir="${HOME}/.local/bin"
fi
mkdir -p "$dest_dir"
install -m 0755 "${tmp}/pier" "${dest_dir}/pier"
echo "installed: ${dest_dir}/pier"
case ":${PATH}:" in
*":${dest_dir}:"*) ;;
*) echo "note: ${dest_dir} is not in your PATH" ;;
esac
