#!/bin/sh
# Package official VMM pairs for both runtime-agent and Builder images.
set -eu
catalog=$1
output=$2
arch=${3:-x86_64}
release_base=${4:-https://github.com/firecracker-microvm/firecracker/releases/download}
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
mkdir -p "$output/versions"
for version in $(jq -er '.versions[]' "$catalog"); do
    case "$version" in *[!0-9.]*|'') echo "invalid release version" >&2; exit 1;; esac
    destination="$output/versions/$version"
    mkdir -p "$destination" "$stage/$version"
    curl --retry 3 --retry-all-errors --connect-timeout 20 --max-time 120 -fsSL \
        "$release_base/v$version/firecracker-v$version-$arch.tgz" -o "$stage/$version.tgz"
    tar -xzf "$stage/$version.tgz" -C "$stage/$version"
    for name in firecracker jailer; do
        install -m 0755 "$stage/$version/release-v$version-$arch/$name-v$version-$arch" "$destination/$name"
    done
    (cd "$destination" && sha256sum firecracker jailer > SHA256SUMS && sha256sum -c SHA256SUMS)
done
# Keep the old entry point until the consumer opts into versioned selection.
legacy=$(jq -er '.legacyVersion' "$catalog")
for name in firecracker jailer SHA256SUMS; do cp "$output/versions/$legacy/$name" "$output/$name"; done
cp "$catalog" "$output/catalog.json"
