#!/bin/sh
# Holdfast's own CA, and the certificates it signs, made as scripts/install.sh makes them (decision 044):
#   holdfast-tls.sh ca FQDN                 /etc/holdfast/tls/ca.key and ca.crt, for 20 years
#   holdfast-tls.sh cert NAME HOST FQDN     NAME.key and NAME.crt (the chain), for HOST and FQDN, for 10 years
set -eu
cd /etc/holdfast/tls
umask 077
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

san_of() { # a subjectAltName entry for a host name or an address
	case "$1" in
	*:*) printf 'IP:%s' "$1" ;;
	*[!0-9.]*) printf 'DNS:%s' "$1" ;;
	*) printf 'IP:%s' "$1" ;;
	esac
}

case "${1:-}" in
ca)
	printf '[req]\ndistinguished_name = dn\nprompt = no\n[dn]\nCN = Holdfast CA (%s)\n[ext]\nbasicConstraints = critical, CA:TRUE, pathlen:0\nkeyUsage = critical, keyCertSign, cRLSign\nsubjectKeyIdentifier = hash\n' "$2" >"$tmp/ca.cnf"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out ca.key 2>/dev/null
	openssl req -x509 -new -config "$tmp/ca.cnf" -extensions ext -key ca.key -sha256 -days 7300 -out ca.crt
	chmod 0644 ca.crt
	;;
cert)
	name=$2 host=$3 here=$4
	san=$(san_of "$host")
	[ "$here" = "$host" ] || san="$san,$(san_of "$here")"
	printf '[req]\ndistinguished_name = dn\nprompt = no\n[dn]\nCN = %s\n[ext]\nbasicConstraints = critical, CA:FALSE\nkeyUsage = critical, digitalSignature\nextendedKeyUsage = serverAuth\nsubjectAltName = %s\nsubjectKeyIdentifier = hash\nauthorityKeyIdentifier = keyid\n' "$host" "$san" >"$tmp/$name.cnf"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$name.key" 2>/dev/null
	openssl req -new -config "$tmp/$name.cnf" -key "$name.key" -out "$tmp/$name.csr"
	openssl x509 -req -in "$tmp/$name.csr" -CA ca.crt -CAkey ca.key -set_serial "0x$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')" \
		-extfile "$tmp/$name.cnf" -extensions ext -sha256 -days 3650 -out "$tmp/$name.leaf" 2>/dev/null
	cat "$tmp/$name.leaf" ca.crt >"$name.crt"
	chmod 0644 "$name.crt"
	;;
*)
	echo "usage: holdfast-tls.sh ca FQDN | cert NAME HOST FQDN" >&2
	exit 2
	;;
esac
