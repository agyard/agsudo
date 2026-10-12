#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo 'Run this installer as root.' >&2
    exit 1
fi
if [ "$#" -ne 1 ]; then
    echo 'Usage: sudo ./scripts/install.sh ALLOWED_NUMERIC_UID' >&2
    exit 2
fi
case "$1" in ''|*[!0-9]*) echo 'Expected a numeric UID.' >&2; exit 2;; esac
agsudo_uid=$(getent passwd "$1" | cut -d: -f3)
if [ -z "$agsudo_uid" ] || [ "$agsudo_uid" = 0 ]; then
    echo 'Choose an existing non-root user UID.' >&2
    exit 2
fi
agsudo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
if [ -x "$agsudo_root/bin/agsudo" ] && [ -x "$agsudo_root/bin/agsudo-helper" ]; then
    agsudo_bin_dir="$agsudo_root/bin"
else
    agsudo_bin_dir="$agsudo_root"
fi
if [ ! -x "$agsudo_bin_dir/agsudo" ] || [ ! -x "$agsudo_bin_dir/agsudo-helper" ]; then
    echo 'Build with make build or unpack a release archive first.' >&2
    exit 1
fi
if [ -L /etc/agsudo/config.json ]; then
    echo 'Refusing a symlink at /etc/agsudo/config.json.' >&2
    exit 1
fi
install -d -o root -g root -m 0755 /usr/local/bin /usr/local/libexec /etc/agsudo
install -d -o root -g root -m 0700 /var/lib/agsudo

# Rename new files into place so an upgrade can coexist with running commands.
agsudo_cli_tmp=$(mktemp /usr/local/bin/.agsudo.XXXXXX)
agsudo_helper_tmp=$(mktemp /usr/local/libexec/.agsudo-helper.XXXXXX)
trap 'rm -f "$agsudo_cli_tmp" "$agsudo_helper_tmp"' EXIT HUP INT TERM
install -o root -g root -m 0755 "$agsudo_bin_dir/agsudo" "$agsudo_cli_tmp"
install -o root -g root -m 4755 "$agsudo_bin_dir/agsudo-helper" "$agsudo_helper_tmp"
mv -f "$agsudo_helper_tmp" /usr/local/libexec/agsudo-helper
mv -f "$agsudo_cli_tmp" /usr/local/bin/agsudo
if [ ! -e /etc/agsudo/config.json ]; then
    umask 077
    cat > /etc/agsudo/config.json <<JSON
{
  "allowed_uids": [$agsudo_uid],
  "reviewer_uids": [$agsudo_uid],
  "state_dir": "/var/lib/agsudo",
  "secure_path": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
  "default_ttl_seconds": 600,
  "max_ttl_seconds": 86400
}
JSON
else
    echo 'Existing configuration preserved. Check allowed_uids and reviewer_uids.'
fi
echo 'Installed. As the configured user, run agsudo doctor and agsudo review --tui.'
