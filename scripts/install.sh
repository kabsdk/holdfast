#!/bin/sh
# Holdfast's installer (decision 044): one line, as root, on each machine, as k3s does it.
#
#   An agent, with the line `holdfast host add` prints:
#     curl -sfL https://github.com/kabsdk/holdfast/releases/latest/download/install.sh | HOLDFAST_SERVER=… HOLDFAST_CODE=… sh -
#   The server:     curl -sfL …/install.sh | sh -s - server      (its settings in the environment: see `help`)
#   The CLI:        curl -sfL …/install.sh | sh -s - cli
#   The witness:    curl -sfL …/install.sh | sh -s - witness     (its settings in the environment)
#
# It checks every setting before it changes anything, and never asks a question: a missing or wrong setting is an
# error that names it, and nothing is changed. Run it again to upgrade: on a machine that is set up already it only
# upgrades the packages. Every package comes from one release and is checked against that release's SHA256SUMS.
set -eu

RELEASES=https://github.com/kabsdk/holdfast/releases
ETC=/etc/holdfast

say() { printf '[holdfast] %s\n' "$*"; }
die() {
	printf '[holdfast] error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Holdfast's installer. Run it as root:  curl -sfL <url>/install.sh | [SETTINGS…] sh -s - [ROLE]

ROLE is agent (the default), server, cli or witness. Settings are environment variables. The installer checks them
all before it changes anything, and stops with the list of what is missing or wrong. On a machine that is set up
already it only upgrades the packages, and ignores the settings.

Every role:
  HOLDFAST_VERSION        the release to install, such as 1.0.0 (default: the latest)
  HOLDFAST_DOWNLOAD_URL   a mirror holding install.sh, VERSION, SHA256SUMS and the packages of one release:
                          https://, http://, or file:/// for a directory here (default: the release on GitHub)

agent: a host to back up. `holdfast host add` prints the whole line.
  HOLDFAST_SERVER         required: the server's URL for agents, such as https://holdfast.example.com:8443
  HOLDFAST_CODE           required: the host's enrollment code (one use, 15 minutes)
  HOLDFAST_CA_SHA256      the pin of the server's certificate (printed by the server's install and by host add)
  HOLDFAST_FQDN           this host's name as added in Holdfast (default: hostname -f)
  On an enrolled host, a code enrolls it again; without one the packages are only upgraded.

server: the Holdfast server, with PostgreSQL. Make the bucket and its two keys first (docs: Storage).
  HOLDFAST_S3_ENDPOINT                      required: the S3 endpoint, such as https://s3.example.com
  HOLDFAST_S3_BUCKET                        required: the bucket, with versioning and a lifecycle rule
  HOLDFAST_GATEWAY_ACCESS_KEY_ID            required: the gateway key, which may add but not delete
  HOLDFAST_GATEWAY_SECRET_ACCESS_KEY        required
  HOLDFAST_MAINTENANCE_ACCESS_KEY_ID        required: the maintenance key, which forgets and prunes
  HOLDFAST_MAINTENANCE_SECRET_ACCESS_KEY    required
  HOLDFAST_ESCROW_RECIPIENT                 required: the age public key (age1…) whose private key you keep
                                            offline; the passwords and the nightly export are encrypted to it
  HOLDFAST_S3_REGION                        the bucket's region, if the S3 server needs one
  HOLDFAST_PUBLIC_URL                       the URL agents use (default: https://<hostname -f>:8443)
  HOLDFAST_ZONE                             the default time zone for schedules (default: this machine's)
  HOLDFAST_DATABASE_DSN                     a PostgreSQL to use (default: install the distribution's PostgreSQL
                                            here, with a database and user for Holdfast)
  HOLDFAST_TLS_CERT, HOLDFAST_TLS_KEY       the agent listener's certificate chain and key (default: make a CA of
                                            its own and a certificate it signs; agents pin the CA)
  Off-site copies, all four or none:
  HOLDFAST_OFFSITE_ENDPOINT, HOLDFAST_OFFSITE_BUCKET, HOLDFAST_OFFSITE_ACCESS_KEY_ID,
  HOLDFAST_OFFSITE_SECRET_ACCESS_KEY        (and HOLDFAST_OFFSITE_REGION if needed)
  The web UI and login, all six or none (without them, use the CLI as root on the server):
  HOLDFAST_UI_URL                           the URL people use, such as https://holdfast.example.com
  HOLDFAST_UI_TLS_CERT, HOLDFAST_UI_TLS_KEY a certificate browsers trust, and its key
  HOLDFAST_OIDC_ISSUER                      your OIDC provider, such as https://auth.example.com
  HOLDFAST_OIDC_CLIENT_ID, HOLDFAST_OIDC_CLIENT_SECRET
  HOLDFAST_OIDC_CA_FILE                     a CA to trust for the provider, if the system does not
  HOLDFAST_ADMINS                           user names that are admin everywhere at first login, comma-separated

cli: the holdfast command, for a workstation. Then: holdfast login --server <the UI's URL>

witness: the storage witness, which lists the bucket for Prometheus apart from the server.
  HOLDFAST_S3_ENDPOINT, HOLDFAST_S3_BUCKET  required, as for the server (and HOLDFAST_S3_REGION)
  HOLDFAST_WITNESS_ACCESS_KEY_ID            required: a key that may only list repos/
  HOLDFAST_WITNESS_SECRET_ACCESS_KEY        required
EOF
}

# Settings are checked first, all of them, and nothing is changed while one is missing or wrong.
missing=
wrong=
need() { # need NAME 'what it is'
	eval "need_v=\${$1:-}"
	[ -n "$need_v" ] || missing="$missing
  $1: $2"
}
wrong() { # wrong NAME 'what is wrong'
	wrong="$wrong
  $1: $2"
}
isset() { # isset NAME: whether the variable is set and not empty
	eval "isset_v=\${$1:-}"
	[ -n "$isset_v" ]
}
stop_if_wrong() {
	[ -n "$missing$wrong" ] || return 0
	[ -z "$missing" ] || printf '[holdfast] error: these settings are missing:%s\n' "$missing" >&2
	[ -z "$wrong" ] || printf '[holdfast] error: these settings are wrong:%s\n' "$wrong" >&2
	printf '[holdfast] Nothing was changed. Set them in the environment and run it again; "sh install.sh help" lists every setting.\n' >&2
	exit 1
}
check_url() { # check_url NAME: an http(s) URL with no spaces
	eval "check_v=\${$1:-}"
	case "$check_v" in
	'') ;;
	*[[:space:]]*) wrong "$1" "has a space in it" ;;
	https://* | http://*) ;;
	*) wrong "$1" "is not a URL: it starts with https://" ;;
	esac
}
check_file() { # check_file NAME: names a file that exists
	eval "check_v=\${$1:-}"
	[ -z "$check_v" ] || [ -s "$check_v" ] || wrong "$1" "$check_v is not a file, or is empty"
}
check_word() { # check_word NAME: no spaces, quotes or line breaks (it goes into a unit file or YAML)
	eval "check_v=\${$1:-}"
	case "$check_v" in *[[:space:]\"\'\\]*) wrong "$1" "has a space, a quote or a backslash in it" ;; esac
}
other_settings() { # whether any setting of this role is in the environment, to say that they are ignored
	env | grep -qE "^HOLDFAST_($1)="
}

preflight() {
	[ "$(id -u)" = 0 ] || die "run it as root: curl … | sudo [SETTINGS…] sh -s - $role"
	case "$(uname -m)" in
	x86_64 | amd64) ;;
	*) die "Holdfast's packages are for x86_64 only; this machine is $(uname -m)" ;;
	esac
	if command -v apt-get >/dev/null 2>&1 && command -v dpkg >/dev/null 2>&1; then
		fmt=deb
	elif command -v dnf >/dev/null 2>&1 && command -v rpm >/dev/null 2>&1; then
		fmt=rpm
	else
		die "neither apt-get nor dnf is here: Holdfast's packages are for Debian, Ubuntu, RHEL, Oracle Linux and their relatives"
	fi
	if [ "$role" != cli ] && [ ! -d /run/systemd/system ]; then
		die "systemd is not running here: Holdfast's services run under systemd"
	fi
	command -v sha256sum >/dev/null 2>&1 || die "sha256sum is missing (coreutils)"
	if command -v curl >/dev/null 2>&1; then
		dl=curl
	elif command -v wget >/dev/null 2>&1; then
		dl=wget
	else
		die "neither curl nor wget is here"
	fi
	case "${HOLDFAST_DOWNLOAD_URL:-}" in
	'' | https://* | http://* | file:///*) ;;
	*) wrong HOLDFAST_DOWNLOAD_URL "is not a URL: https://, http:// or file:/// for a directory on this machine" ;;
	esac
	case "${HOLDFAST_DOWNLOAD_URL:-}" in *[[:space:]]*) wrong HOLDFAST_DOWNLOAD_URL "has a space in it" ;; esac
	case "${HOLDFAST_VERSION:-}" in *[!0-9A-Za-z.~+-]*) wrong HOLDFAST_VERSION "is not a version, such as 1.0.0" ;; esac
}

fetch() { # fetch URL FILE
	if [ "$dl" = curl ]; then
		curl -fsSL --retry 3 -o "$2" "$1" || die "could not download $1"
	else
		wget -q -O "$2" "$1" || die "could not download $1"
	fi
}

# packages NAME…: download the Holdfast packages of one release, check each against its SHA256SUMS, and install them.
packages() {
	stop_if_wrong
	base=${HOLDFAST_DOWNLOAD_URL:-}
	if [ -z "$base" ]; then
		if [ -n "${HOLDFAST_VERSION:-}" ]; then base=$RELEASES/download/v$HOLDFAST_VERSION; else base=$RELEASES/latest/download; fi
	fi
	base=${base%/}
	fetch "$base/VERSION" "$tmp/VERSION"
	pkgver=$(tr -d ' \r\n' <"$tmp/VERSION")
	case "$pkgver" in '' | *[!0-9A-Za-z.~+-]*) die "$base/VERSION does not hold a version" ;; esac
	if [ -n "${HOLDFAST_VERSION:-}" ] && [ "${pkgver%-*}" != "$HOLDFAST_VERSION" ]; then
		die "$base holds Holdfast $pkgver, not $HOLDFAST_VERSION"
	fi
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
	files=
	for p in "$@"; do
		if [ "$fmt" = deb ]; then f=holdfast-${p}_${pkgver}_amd64.deb; else f=holdfast-$p-$pkgver.x86_64.rpm; fi
		want=$(awk -v f="$f" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
		[ -n "$want" ] || die "$f is not in $base/SHA256SUMS"
		fetch "$base/$f" "$tmp/$f"
		got=$(sha256sum "$tmp/$f" | cut -d' ' -f1)
		[ "$got" = "$want" ] || die "$f does not match SHA256SUMS: it is not installed"
		files="$files $tmp/$f"
	done
	say "installing Holdfast $pkgver:$(for p in "$@"; do printf ' holdfast-%s' "$p"; done)"
	# The packages depend only on each other, so no repository is needed for them.
	if [ "$fmt" = deb ]; then
		# shellcheck disable=SC2086
		DEBIAN_FRONTEND=noninteractive apt-get install -y -q $files >"$tmp/log" 2>&1 || {
			cat "$tmp/log" >&2
			die "apt-get could not install the packages"
		}
	else
		# shellcheck disable=SC2086
		dnf install -y -q --disablerepo='*' $files >"$tmp/log" 2>&1 || {
			cat "$tmp/log" >&2
			die "dnf could not install the packages"
		}
	fi
}

# distro PACKAGE…: install packages of the distribution itself, such as PostgreSQL.
distro() {
	if [ "$fmt" = deb ]; then
		if [ -z "${apt_updated:-}" ]; then
			apt-get update -q >"$tmp/log" 2>&1 || {
				cat "$tmp/log" >&2
				die "apt-get update failed"
			}
			apt_updated=1
		fi
		DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" >"$tmp/log" 2>&1 || {
			cat "$tmp/log" >&2
			die "apt-get could not install $*"
		}
	else
		dnf install -y -q "$@" >"$tmp/log" 2>&1 || {
			cat "$tmp/log" >&2
			die "dnf could not install $*"
		}
	fi
}

open_port() { # open_port PORT: in firewalld or ufw, if one of them is on
	if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd -q --state 2>/dev/null; then
		firewall-cmd -q --permanent --add-port="$1/tcp" && firewall-cmd -q --reload && say "opened $1/tcp in firewalld"
	elif command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		ufw allow "$1/tcp" >/dev/null && say "opened $1/tcp in ufw"
	fi
}

fqdn_here() { hostname -f 2>/dev/null || uname -n; }
url_hostport() { printf '%s' "$1" | sed -E 's#^[A-Za-z]+://##; s#/.*$##'; }
url_host() { url_hostport "$1" | sed -E 's#^\[(.*)\](:[0-9]+)?$#\1#; t; s#:[0-9]+$##'; }
url_port() {
	case "$(url_hostport "$1")" in
	*\]:[0-9]* | [!\[]*:[0-9]*) url_hostport "$1" | sed -E 's#.*:##' ;;
	*) case "$1" in https:*) echo 443 ;; *) echo 80 ;; esac ;;
	esac
}
san_of() { # a subjectAltName entry for a host name or an address
	case "$1" in
	*:* | *[!0-9.]*) case "$1" in *:*) printf 'IP:%s' "$1" ;; *) printf 'DNS:%s' "$1" ;; esac ;;
	*) printf 'IP:%s' "$1" ;;
	esac
}
q() { printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"; } # a YAML string
write_key() { # write_key FILE ACCESS_KEY_ID SECRET: an S3 key file, root only; the secret never goes into argv
	(
		umask 077
		printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' "$2" "$3" >"$1"
	)
}
pin_of() { # pin_of FILE: the sha256 of the last certificate in a PEM chain, which agents pin
	awk '/-----BEGIN CERTIFICATE-----/ { c = "" } { c = c $0 "\n" } /-----END CERTIFICATE-----/ { last = c } END { printf "%s", last }' "$1" |
		openssl x509 -outform DER | sha256sum | cut -c1-64
}

# ---------------------------------------------------------------------------------------------------------- agent
role_agent() {
	enrolled=false
	[ ! -s $ETC/agent.yaml ] || enrolled=true
	if ! $enrolled || [ -n "${HOLDFAST_CODE:-}" ]; then
		need HOLDFAST_SERVER "the server's URL for agents, such as https://holdfast.example.com:8443 (holdfast host add prints the whole line)"
		need HOLDFAST_CODE "the host's enrollment code, from holdfast host add or holdfast host enroll-code <fqdn>"
	fi
	case "${HOLDFAST_SERVER:-https://}" in https://*) ;; *) wrong HOLDFAST_SERVER "must start with https://" ;; esac
	if [ -n "${HOLDFAST_CA_SHA256:-}" ] && ! printf '%s' "$HOLDFAST_CA_SHA256" | grep -qE '^[0-9a-fA-F]{64}$'; then
		wrong HOLDFAST_CA_SHA256 "is not a sha256 (64 hex digits)"
	fi
	fqdn=${HOLDFAST_FQDN:-$(fqdn_here)}
	case "$fqdn" in *.*) ;; *) wrong HOLDFAST_FQDN "this host's name '$fqdn' is not a fully qualified name: set HOLDFAST_FQDN" ;; esac
	stop_if_wrong
	packages restic agent
	if [ -n "${HOLDFAST_CODE:-}" ]; then
		set -- enroll --server "$HOLDFAST_SERVER" --fqdn "$fqdn" --code "$HOLDFAST_CODE"
		[ -z "${HOLDFAST_CA_SHA256:-}" ] || set -- "$@" --ca-sha256 "$HOLDFAST_CA_SHA256"
		holdfast-agent "$@" || die "the enrollment failed (above). A code works once, for 15 minutes: holdfast host enroll-code $fqdn gives a new one"
		systemctl enable -q holdfast-agent
		systemctl restart holdfast-agent
		say "enrolled $fqdn with $HOLDFAST_SERVER. The agent runs, and backs up on its jobs' schedules."
	else
		other_settings 'SERVER|FQDN|CA_SHA256' && say "this host is enrolled already: HOLDFAST_SERVER and the like are ignored"
		say "upgraded the agent to $pkgver; it was restarted if it ran"
	fi
}

# ---------------------------------------------------------------------------------------------------------- server
wait_server() {
	i=0
	until holdfast status >/dev/null 2>&1; do
		i=$((i + 1))
		if [ $i -gt 60 ]; then
			journalctl -u holdfast-server -n 40 --no-pager >&2 || true
			die "holdfast-server did not start within a minute: its log is above"
		fi
		sleep 1
	done
}

local_postgresql() { # local_postgresql [PASSWORD]: the distribution's PostgreSQL, a user holdfast with PASSWORD (or a new one)
	say "installing PostgreSQL from the distribution, with a database and user for Holdfast"
	if [ "$fmt" = deb ]; then
		distro postgresql
	else
		# RHEL 9 and its relatives offer newer PostgreSQL as module streams; Holdfast is tested with 16.
		if dnf -q module list postgresql 2>/dev/null | grep -qE '^postgresql +16 '; then
			dnf -y -q module enable postgresql:16 >"$tmp/log" 2>&1 || {
				cat "$tmp/log" >&2
				die "could not enable the module stream postgresql:16"
			}
		fi
		distro postgresql-server
		if [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then
			postgresql-setup --initdb >"$tmp/log" 2>&1 || {
				cat "$tmp/log" >&2
				die "postgresql-setup --initdb failed"
			}
		fi
		# Its pg_hba.conf lets local TCP logins in by ident; Holdfast logs in with a password.
		sed -i -E 's/^(host[[:space:]]+all[[:space:]]+all[[:space:]]+(127\.0\.0\.1\/32|::1\/128)[[:space:]]+)ident/\1scram-sha-256/' /var/lib/pgsql/data/pg_hba.conf
	fi
	systemctl enable -q postgresql
	systemctl restart postgresql
	i=0
	until (cd / && runuser -u postgres -- psql -X -q -c 'SELECT 1') >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -le 60 ] || die "PostgreSQL did not start"
		sleep 1
	done
	pw=${1:-$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')}
	# The password goes to psql on stdin, never in argv.
	(cd / && runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1) >"$tmp/log" 2>&1 <<EOF || {
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'holdfast') THEN CREATE ROLE holdfast LOGIN; END IF;
END \$\$;
ALTER ROLE holdfast PASSWORD '$pw';
EOF
		cat "$tmp/log" >&2
		die "could not make PostgreSQL's user for Holdfast"
	}
	if [ -z "$(cd / && runuser -u postgres -- psql -X -tA -c "SELECT 1 FROM pg_database WHERE datname = 'holdfast'")" ]; then
		(cd / && runuser -u postgres -- createdb -O holdfast holdfast) || die "could not make PostgreSQL's database for Holdfast"
	fi
	(
		umask 077
		printf 'postgres://holdfast:%s@127.0.0.1:5432/holdfast?sslmode=disable\n' "$pw" >$ETC/db.dsn
	)
}

make_tls() { # the agent listener's own CA, and a certificate it signs for the public URL's name
	host=$(url_host "$public_url")
	san=$(san_of "$host")
	here=$(fqdn_here)
	[ "$here" = "$host" ] || san="$san,$(san_of "$here")"
	(
		umask 077
		mkdir -p $ETC/tls
		cd $ETC/tls
		if [ ! -s ca.key ] || [ ! -s ca.crt ]; then
			printf '[req]\ndistinguished_name = dn\nprompt = no\n[dn]\nCN = Holdfast agent CA (%s)\n[ext]\nbasicConstraints = critical, CA:TRUE, pathlen:0\nkeyUsage = critical, keyCertSign, cRLSign\nsubjectKeyIdentifier = hash\n' "$host" >"$tmp/ca.cnf"
			openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out ca.key 2>/dev/null
			openssl req -x509 -new -config "$tmp/ca.cnf" -extensions ext -key ca.key -sha256 -days 7300 -out ca.crt
			chmod 0644 ca.crt
		fi
		printf '[req]\ndistinguished_name = dn\nprompt = no\n[dn]\nCN = %s\n[ext]\nbasicConstraints = critical, CA:FALSE\nkeyUsage = critical, digitalSignature\nextendedKeyUsage = serverAuth\nsubjectAltName = %s\nsubjectKeyIdentifier = hash\nauthorityKeyIdentifier = keyid\n' "$host" "$san" >"$tmp/leaf.cnf"
		openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out server.key 2>/dev/null
		openssl req -new -config "$tmp/leaf.cnf" -key server.key -out "$tmp/leaf.csr"
		openssl x509 -req -in "$tmp/leaf.csr" -CA ca.crt -CAkey ca.key -set_serial "0x$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')" \
			-extfile "$tmp/leaf.cnf" -extensions ext -sha256 -days 3650 -out "$tmp/leaf.crt" 2>/dev/null
		cat "$tmp/leaf.crt" ca.crt >server.crt
		chmod 0644 server.crt
	)
}

# A lost server's replacement (decision 040): /etc/holdfast was put back from its offline copy on a new machine. Install
# what it needs, with the database's password from the copy, and leave the server stopped: restore-export loads the
# nightly export into the empty database first.
prepare_rebuild() {
	stop_if_wrong
	dsn=$(cat $ETC/db.dsn 2>/dev/null || true)
	case "$dsn" in
	postgres://holdfast:*@127.0.0.1:5432/holdfast\?*)
		pw=${dsn#postgres://holdfast:}
		local_postgresql "${pw%%@*}"
		;;
	*) say "the database in $ETC/db.dsn is not one this installer made here: make it, empty, before the restore" ;;
	esac
	packages restic cli server
	for p in $(sed -nE 's/^[[:space:]]*listen: *"?[^:"]*:([0-9]+)"?.*/\1/p' $ETC/server.yaml); do open_port "$p"; done
	cat <<EOF
[holdfast] $ETC is back and Holdfast $pkgver is installed; the server is not started.
  Restore the newest nightly export into the empty database, then start it:
    holdfast-server restore-export -identity /path/to/escrow-key.txt -latest
    systemctl enable --now holdfast-server
  The agents come back by themselves. docs: Operating Holdfast, a lost server.
EOF
}

role_server() {
	if [ -s $ETC/server.yaml ] && ! command -v holdfast-server >/dev/null 2>&1; then
		prepare_rebuild
		return 0
	fi
	if [ -s $ETC/server.yaml ]; then
		packages restic cli server
		other_settings 'S3_[A-Z]+|GATEWAY_[A-Z_]+|MAINTENANCE_[A-Z_]+|ESCROW_RECIPIENT|PUBLIC_URL|DATABASE_DSN|UI_[A-Z_]+|OIDC_[A-Z_]+|OFFSITE_[A-Z_]+' &&
			say "the server is set up already: the settings are ignored; change them in $ETC/server.yaml"
		wait_server
		say "upgraded the server to $pkgver; it migrated its database as it started. Upgrade the agents now, in any order."
		return 0
	fi

	need HOLDFAST_S3_ENDPOINT "the S3 endpoint, such as https://s3.example.com"
	need HOLDFAST_S3_BUCKET "the bucket, with versioning on and a lifecycle rule (docs: Storage)"
	need HOLDFAST_GATEWAY_ACCESS_KEY_ID "the gateway key's ID: it may read and add under repos/, and delete only lock files"
	need HOLDFAST_GATEWAY_SECRET_ACCESS_KEY "the gateway key's secret"
	need HOLDFAST_MAINTENANCE_ACCESS_KEY_ID "the maintenance key's ID: it forgets and prunes, and may not delete old versions"
	need HOLDFAST_MAINTENANCE_SECRET_ACCESS_KEY "the maintenance key's secret"
	need HOLDFAST_ESCROW_RECIPIENT "an age public key (age1…), made offline with age-keygen; without it there is no nightly export to rebuild a lost server from"
	check_url HOLDFAST_S3_ENDPOINT
	check_word HOLDFAST_S3_BUCKET
	check_word HOLDFAST_S3_REGION
	case "${HOLDFAST_ESCROW_RECIPIENT:-age1}" in age1*) ;; *) wrong HOLDFAST_ESCROW_RECIPIENT "is not an age public key: it starts with age1" ;; esac
	public_url=${HOLDFAST_PUBLIC_URL:-https://$(fqdn_here):8443}
	case "$public_url" in https://*) ;; *) wrong HOLDFAST_PUBLIC_URL "must start with https://: agents log in with TLS" ;; esac
	check_url HOLDFAST_PUBLIC_URL
	zone=${HOLDFAST_ZONE:-}
	if [ -n "$zone" ]; then
		[ -f "/usr/share/zoneinfo/$zone" ] || wrong HOLDFAST_ZONE "$zone is not a time zone here (/usr/share/zoneinfo)"
	else
		zone=$(timedatectl show -p Timezone --value 2>/dev/null || true)
		[ -n "$zone" ] || zone=$(readlink /etc/localtime 2>/dev/null | sed -n 's#.*/zoneinfo/##p')
		[ -n "$zone" ] && [ -f "/usr/share/zoneinfo/$zone" ] || zone=UTC
	fi
	if isset HOLDFAST_TLS_CERT || isset HOLDFAST_TLS_KEY; then
		need HOLDFAST_TLS_CERT "the agent listener's certificate chain, with its CA last (with HOLDFAST_TLS_KEY)"
		need HOLDFAST_TLS_KEY "the agent listener's private key (with HOLDFAST_TLS_CERT)"
		check_file HOLDFAST_TLS_CERT
		check_file HOLDFAST_TLS_KEY
	fi
	offsite=false
	if isset HOLDFAST_OFFSITE_ENDPOINT || isset HOLDFAST_OFFSITE_BUCKET || isset HOLDFAST_OFFSITE_ACCESS_KEY_ID || isset HOLDFAST_OFFSITE_SECRET_ACCESS_KEY; then
		offsite=true
		need HOLDFAST_OFFSITE_ENDPOINT "the off-site S3 endpoint (off-site copies need all four HOLDFAST_OFFSITE_ settings)"
		need HOLDFAST_OFFSITE_BUCKET "the off-site bucket, with versioning on and a lifecycle rule"
		need HOLDFAST_OFFSITE_ACCESS_KEY_ID "the off-site key's ID: a key of its own, with the maintenance policy"
		need HOLDFAST_OFFSITE_SECRET_ACCESS_KEY "the off-site key's secret"
		check_url HOLDFAST_OFFSITE_ENDPOINT
		check_word HOLDFAST_OFFSITE_BUCKET
		check_word HOLDFAST_OFFSITE_REGION
		if [ "${HOLDFAST_OFFSITE_ENDPOINT:-}" = "${HOLDFAST_S3_ENDPOINT:-}" ] && [ "${HOLDFAST_OFFSITE_BUCKET:-}" = "${HOLDFAST_S3_BUCKET:-}" ]; then
			wrong HOLDFAST_OFFSITE_BUCKET "is the primary bucket: off-site copies need a bucket of their own"
		fi
	fi
	ui=false
	if isset HOLDFAST_UI_URL || isset HOLDFAST_OIDC_ISSUER || isset HOLDFAST_OIDC_CLIENT_ID; then
		ui=true
		need HOLDFAST_UI_URL "the URL people use, such as https://holdfast.example.com (the UI needs all six UI and OIDC settings)"
		need HOLDFAST_UI_TLS_CERT "a certificate for the UI that browsers trust"
		need HOLDFAST_UI_TLS_KEY "its private key"
		need HOLDFAST_OIDC_ISSUER "your OIDC provider's issuer URL"
		need HOLDFAST_OIDC_CLIENT_ID "Holdfast's client ID at the provider"
		need HOLDFAST_OIDC_CLIENT_SECRET "its client secret"
		case "${HOLDFAST_UI_URL:-https://}" in https://*) ;; *) wrong HOLDFAST_UI_URL "must start with https://" ;; esac
		check_url HOLDFAST_UI_URL
		check_url HOLDFAST_OIDC_ISSUER
		check_file HOLDFAST_UI_TLS_CERT
		check_file HOLDFAST_UI_TLS_KEY
		check_file HOLDFAST_OIDC_CA_FILE
		check_word HOLDFAST_OIDC_CLIENT_ID
		case "${HOLDFAST_ADMINS:-}" in *[!A-Za-z0-9._@,-]*) wrong HOLDFAST_ADMINS "user names, comma-separated, with no spaces" ;; esac
	fi
	stop_if_wrong

	install -d -m 0700 $ETC
	install -d -m 0700 $ETC/tls
	command -v openssl >/dev/null 2>&1 || distro openssl
	[ -n "${HOLDFAST_DATABASE_DSN:-}" ] || local_postgresql
	packages restic cli server
	if [ -n "${HOLDFAST_DATABASE_DSN:-}" ]; then
		(
			umask 077
			printf '%s\n' "$HOLDFAST_DATABASE_DSN" >$ETC/db.dsn
		)
	fi
	[ -s $ETC/credentials.key ] || holdfast-server keygen $ETC/credentials.key >/dev/null
	write_key $ETC/gateway.env "$HOLDFAST_GATEWAY_ACCESS_KEY_ID" "$HOLDFAST_GATEWAY_SECRET_ACCESS_KEY"
	write_key $ETC/maintenance.env "$HOLDFAST_MAINTENANCE_ACCESS_KEY_ID" "$HOLDFAST_MAINTENANCE_SECRET_ACCESS_KEY"
	if [ -n "${HOLDFAST_TLS_CERT:-}" ]; then
		install -m 0644 "$HOLDFAST_TLS_CERT" $ETC/tls/server.crt
		install -m 0600 "$HOLDFAST_TLS_KEY" $ETC/tls/server.key
	else
		make_tls
	fi
	pin=$(pin_of $ETC/tls/server.crt)
	listen=$(url_port "$public_url")
	if $offsite; then
		write_key $ETC/offsite.env "$HOLDFAST_OFFSITE_ACCESS_KEY_ID" "$HOLDFAST_OFFSITE_SECRET_ACCESS_KEY"
	fi
	if $ui; then
		install -m 0644 "$HOLDFAST_UI_TLS_CERT" $ETC/tls/ui.crt
		install -m 0600 "$HOLDFAST_UI_TLS_KEY" $ETC/tls/ui.key
		[ -z "${HOLDFAST_OIDC_CA_FILE:-}" ] || install -m 0644 "$HOLDFAST_OIDC_CA_FILE" $ETC/tls/oidc-ca.crt
		(
			umask 077
			printf '%s\n' "$HOLDFAST_OIDC_CLIENT_SECRET" >$ETC/oidc-client-secret
		)
	fi

	{
		printf '# Written by Holdfast'"'"'s installer on %s (decision 044). Edit it as root, then: systemctl restart holdfast-server\n' "$(date -u +%Y-%m-%d)"
		printf '# Every setting: /usr/share/holdfast/server.yaml.example\n\n'
		printf 'listen: ":%s"                 # agents, their backups (the gateway), /metrics\n' "$listen"
		printf 'public_url: %s\n' "$(q "$public_url")"
		printf 'tls:\n  cert_file: %s/tls/server.crt\n  key_file: %s/tls/server.key\n' $ETC $ETC
		printf 'ca_sha256: %s   # agents pin this certificate\n' "$(q "$pin")"
		printf 'api_socket: /run/holdfast/api.sock\n'
		printf 'database_dsn_file: %s/db.dsn\n' $ETC
		printf 'credentials_key_file: %s/credentials.key   # keep a copy offline\n' $ETC
		printf 'default_zone: %s\n' "$(q "$zone")"
		[ -z "${HOLDFAST_DOWNLOAD_URL:-}" ] || printf 'download_url: %s   # where hosts fetch install.sh and the packages\n' "$(q "${HOLDFAST_DOWNLOAD_URL%/}")"
		printf '\nstorage:\n  endpoint: %s\n  bucket: %s\n' "$(q "$HOLDFAST_S3_ENDPOINT")" "$(q "$HOLDFAST_S3_BUCKET")"
		[ -z "${HOLDFAST_S3_REGION:-}" ] || printf '  region: %s\n' "$(q "$HOLDFAST_S3_REGION")"
		printf '  maintenance_credentials_file: %s/maintenance.env\n  gateway_credentials_file: %s/gateway.env\n' $ETC $ETC
		printf '\nescrow:\n  age_recipient: %s\n' "$(q "$HOLDFAST_ESCROW_RECIPIENT")"
		if $offsite; then
			printf '\noffsite:\n  endpoint: %s\n  bucket: %s\n' "$(q "$HOLDFAST_OFFSITE_ENDPOINT")" "$(q "$HOLDFAST_OFFSITE_BUCKET")"
			[ -z "${HOLDFAST_OFFSITE_REGION:-}" ] || printf '  region: %s\n' "$(q "$HOLDFAST_OFFSITE_REGION")"
			printf '  maintenance_credentials_file: %s/offsite.env\n' $ETC
		fi
		if $ui; then
			printf '\nui:\n  listen: ":%s"\n  public_url: %s\n' "$(url_port "$HOLDFAST_UI_URL")" "$(q "${HOLDFAST_UI_URL%/}")"
			printf '  tls:\n    cert_file: %s/tls/ui.crt\n    key_file: %s/tls/ui.key\n' $ETC $ETC
			printf '\nauth:\n  oidc:\n    issuer: %s\n    client_id: %s\n' "$(q "$HOLDFAST_OIDC_ISSUER")" "$(q "$HOLDFAST_OIDC_CLIENT_ID")"
			printf '    client_secret_file: %s/oidc-client-secret\n' $ETC
			[ -z "${HOLDFAST_OIDC_CA_FILE:-}" ] || printf '    ca_file: %s/tls/oidc-ca.crt\n' $ETC
			printf '  bootstrap_admins: ['
			sep=
			for a in $(printf '%s' "${HOLDFAST_ADMINS:-}" | tr ',' ' '); do
				printf '%s%s' "$sep" "$(q "$a")"
				sep=', '
			done
			printf ']\n'
		fi
	} >"$tmp/server.yaml"
	install -m 0600 "$tmp/server.yaml" $ETC/server.yaml

	open_port "$listen"
	! $ui || open_port "$(url_port "$HOLDFAST_UI_URL")"
	systemctl enable -q holdfast-server
	systemctl restart holdfast-server
	wait_server

	cat <<EOF
[holdfast] The Holdfast server $pkgver runs.
  Agents connect to   $public_url
  Agents pin          $pin
  On this host, as root, the CLI works now:  holdfast status
EOF
	if $ui; then echo "  People log in at    ${HOLDFAST_UI_URL%/}"; else
		echo "  The web UI is off until you set ui: and auth: in $ETC/server.yaml (docs: Installation, the web UI)."
	fi
	cat <<EOF

  Copy $ETC to a safe place offline now, and keep the escrow's age private key with it: a new server is
  rebuilt from them if this one is lost. It holds the credentials key, the storage keys, the agents' CA and
  server.yaml.

  Next, add a host. It prints the one line to run on that host:
    holdfast host add web01.example.com --job system --path /etc --daily 02:00 \\
      --keep-daily 14d --keep-weekly 8w --keep-monthly 12mo --keep-yearly 3y
EOF
}

# ---------------------------------------------------------------------------------------------------------- cli
role_cli() {
	stop_if_wrong
	packages cli
	say "installed the holdfast command. Log in with: holdfast login --server <the UI's URL>"
}

# ---------------------------------------------------------------------------------------------------------- witness
role_witness() {
	if [ -s $ETC/witness.env ]; then
		packages freshness
		other_settings 'S3_[A-Z]+|WITNESS_[A-Z_]+' && say "the witness is set up already: the settings are ignored"
		say "upgraded the storage witness to $pkgver"
		return 0
	fi
	need HOLDFAST_S3_ENDPOINT "the S3 endpoint, such as https://s3.example.com"
	need HOLDFAST_S3_BUCKET "the bucket Holdfast backs up to"
	need HOLDFAST_WITNESS_ACCESS_KEY_ID "the witness key's ID: a key that may only list repos/"
	need HOLDFAST_WITNESS_SECRET_ACCESS_KEY "the witness key's secret"
	check_url HOLDFAST_S3_ENDPOINT
	check_word HOLDFAST_S3_BUCKET
	check_word HOLDFAST_S3_REGION
	stop_if_wrong
	packages freshness
	write_key $ETC/witness.env "$HOLDFAST_WITNESS_ACCESS_KEY_ID" "$HOLDFAST_WITNESS_SECRET_ACCESS_KEY"
	region=
	[ -z "${HOLDFAST_S3_REGION:-}" ] || region=" -region $HOLDFAST_S3_REGION"
	mkdir -p /etc/systemd/system/holdfast-freshness.service.d
	{
		printf '# Written by Holdfast'"'"'s installer (decision 044): where the witness looks.\n[Service]\nExecStart=\n'
		printf 'ExecStart=/usr/bin/holdfast-freshness -endpoint %s -bucket %s%s -credentials-file ${CREDENTIALS_DIRECTORY}/witness.env -listen :9817\n' \
			"$HOLDFAST_S3_ENDPOINT" "$HOLDFAST_S3_BUCKET" "$region"
	} >/etc/systemd/system/holdfast-freshness.service.d/install.conf
	systemctl daemon-reload
	systemctl enable -q holdfast-freshness
	systemctl restart holdfast-freshness
	open_port 9817
	say "the storage witness runs. Point Prometheus at http://$(fqdn_here):9817/metrics, with the rules in /usr/share/holdfast/prometheus/ on the server."
}

# ---------------------------------------------------------------------------------------------------------- main
role=${1:-agent}
case "$role" in
help | -h | --help)
	usage
	exit 0
	;;
agent | server | cli | witness) ;;
*) die "'$role' is not a role: agent, server, cli or witness (sh install.sh help)" ;;
esac
preflight
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM
"role_$role"
