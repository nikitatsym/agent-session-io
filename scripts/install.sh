#!/bin/sh

set -eu

repository="nikitatsym/agent-session-io"
release="${SESSIONIO_VERSION:-latest}"
install_dir="${SESSIONIO_INSTALL_DIR:-"$HOME/.local/bin"}"

fail() {
    printf 'sessionio installer: %s\n' "$1" >&2
    exit 1
}

download() {
    source_url="$1"
    destination="$2"
    if command -v curl >/dev/null 2>&1; then
        curl --proto '=https' --tlsv1.2 -fsSL "$source_url" -o "$destination"
        return
    fi
    if command -v wget >/dev/null 2>&1; then
        wget -q "$source_url" -O "$destination"
        return
    fi
    fail "curl or wget is required"
}

case "$(uname -s)" in
    Darwin) operating_system="darwin" ;;
    Linux) operating_system="linux" ;;
    *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
    x86_64 | amd64) architecture="amd64" ;;
    arm64 | aarch64) architecture="arm64" ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
esac

archive="sessionio_${operating_system}_${architecture}.tar.gz"
if [ "$release" = "latest" ]; then
    release_url="https://github.com/${repository}/releases/latest/download"
else
    case "$release" in
        v*) ;;
        *) release="v${release}" ;;
    esac
    release_url="https://github.com/${repository}/releases/download/${release}"
fi

temporary_dir="$(mktemp -d)"
cleanup() {
    rm -rf "$temporary_dir"
}
trap cleanup EXIT HUP INT TERM

download "${release_url}/${archive}" "${temporary_dir}/${archive}"
download "${release_url}/checksums.txt" "${temporary_dir}/checksums.txt"

expected_hash="$(awk -v name="$archive" '$2 == name { print $1 }' "${temporary_dir}/checksums.txt")"
[ -n "$expected_hash" ] || fail "archive checksum is missing"

if command -v sha256sum >/dev/null 2>&1; then
    actual_hash="$(sha256sum "${temporary_dir}/${archive}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
    actual_hash="$(shasum -a 256 "${temporary_dir}/${archive}" | awk '{ print $1 }')"
else
    fail "sha256sum or shasum is required"
fi

[ "$actual_hash" = "$expected_hash" ] || fail "archive checksum mismatch"

tar -xzf "${temporary_dir}/${archive}" -C "$temporary_dir"
[ -f "${temporary_dir}/sessionio" ] || fail "archive does not contain sessionio"

mkdir -p "$install_dir"
cp "${temporary_dir}/sessionio" "${install_dir}/sessionio"
chmod 0755 "${install_dir}/sessionio"

printf 'installed sessionio to %s/sessionio\n' "$install_dir"

if [ "${SESSIONIO_NO_COMPLETION:-0}" != "1" ]; then
    completion_help="$("${install_dir}/sessionio" completion --help 2>/dev/null)"
    case "$completion_help" in
        *"Install completion into the current shell"*) completion_supported=1 ;;
        *) completion_supported=0 ;;
    esac
    if [ "$completion_supported" = "1" ]; then
        completion_shell="${SESSIONIO_COMPLETION_SHELL:-}"
        if [ -n "$completion_shell" ]; then
            "${install_dir}/sessionio" completion install "$completion_shell"
        else
            "${install_dir}/sessionio" completion install
        fi
    else
        printf 'completion setup is unavailable in this release; install a newer release to enable it\n' >&2
    fi
fi

case ":${PATH}:" in
    *":${install_dir}:"*) ;;
    *) printf 'add %s to PATH to run sessionio\n' "$install_dir" ;;
esac
