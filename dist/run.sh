#!/bin/sh
# Launcher for the ebeco-spot LaunchAgent.
#
# Reads the Ebeco credentials from the login Keychain at start-up so they never
# live in the plist or on disk, then execs the binary that sits next to this
# script. Set the items up once with:
#
#   security add-generic-password -U -a "$USER" -s ebeco-spot-email    -T /usr/bin/security -w
#   security add-generic-password -U -a "$USER" -s ebeco-spot-password -T /usr/bin/security -w
#
# (-w with no value prompts so the secret stays out of your shell history;
#  -T /usr/bin/security lets this script read it unattended, no GUI dialog.)
set -eu

here=$(cd "$(dirname "$0")" && pwd)

EBECO_EMAIL=$(security find-generic-password -s ebeco-spot-email -w) || {
	echo "ebeco-spot: missing Keychain item 'ebeco-spot-email'" >&2
	exit 78 # EX_CONFIG
}
EBECO_PASSWORD=$(security find-generic-password -s ebeco-spot-password -w) || {
	echo "ebeco-spot: missing Keychain item 'ebeco-spot-password'" >&2
	exit 78
}
export EBECO_EMAIL EBECO_PASSWORD

exec "$here/ebeco-spot" -config "$here/config.toml"
