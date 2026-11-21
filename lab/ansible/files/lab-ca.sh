#!/usr/bin/env bash
# lab-ca.sh <dir> <CA name> <server name> <subjectAltName>: the lab's own CA and one server certificate it signs, in <dir>
# in the lab's state, made once and made again only when needed. lab/ansible/site.yml runs it on this host for hfsrv.
#   ca.key (0600), ca.crt             the CA, ECDSA P-256, 10 years; made once
#   ca.sha256                         sha256 of ca.crt's DER, lowercase hex: server.yaml's ca_sha256, the pin agents check
#   <name>.key (0600), <name>.crt     the server's key and certificate (serverAuth, the SANs given), 825 days; made again
#                                     when it is missing, does not verify against ca.crt, expires within 30 days, or was
#                                     made for other SANs (<name>.san keeps the ones it was made for)
#   <name>.chain.crt                  the certificate, then ca.crt: what the server sends, so the pinned CA is in the chain
# It prints "made ca", "made cert <name>" for what it changed, then "pin <sha256>". No private key is ever printed.
set -euo pipefail
dir=$1 ca_name=$2 name=$3 san=$4
[[ $name =~ ^[a-z0-9.-]+$ ]] || { echo "lab-ca.sh: '$name' is not a host name" >&2; exit 2; }
umask 077
mkdir -p "$dir"
cd "$dir"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [[ ! -s ca.key || ! -s ca.crt ]]; then
	cat >"$tmp/ca.cnf" <<EOF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = $ca_name
[ext]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
EOF
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/ca.key" 2>/dev/null
	openssl req -x509 -new -config "$tmp/ca.cnf" -extensions ext -key "$tmp/ca.key" -sha256 -days 3650 -out "$tmp/ca.crt"
	mv "$tmp/ca.key" ca.key
	install -m 0644 "$tmp/ca.crt" ca.crt
	rm -f "$name.crt" # signed by a CA that is gone
	echo "made ca"
fi
openssl x509 -in ca.crt -outform DER | sha256sum | cut -c1-64 >"$tmp/pin"
install -m 0644 "$tmp/pin" ca.sha256

fresh=1
if [[ ! -s $name.key || ! -s $name.crt || ! -s $name.chain.crt || $(cat "$name.san" 2>/dev/null) != "$san" ]]; then
	fresh=0
elif ! openssl verify -CAfile ca.crt "$name.crt" >/dev/null 2>&1; then
	fresh=0
elif ! openssl x509 -in "$name.crt" -noout -checkend 2592000 >/dev/null; then
	fresh=0
fi
if ((fresh == 0)); then
	cat >"$tmp/leaf.cnf" <<EOF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = $name
[ext]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = $san
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
EOF
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/leaf.key" 2>/dev/null
	openssl req -new -config "$tmp/leaf.cnf" -key "$tmp/leaf.key" -out "$tmp/leaf.csr"
	openssl x509 -req -in "$tmp/leaf.csr" -CA ca.crt -CAkey ca.key -set_serial "0x$(openssl rand -hex 16)" \
		-extfile "$tmp/leaf.cnf" -extensions ext -sha256 -days 825 -out "$tmp/leaf.crt" 2>/dev/null
	openssl verify -CAfile ca.crt "$tmp/leaf.crt" >/dev/null
	mv "$tmp/leaf.key" "$name.key"
	install -m 0644 "$tmp/leaf.crt" "$name.crt"
	cat "$name.crt" ca.crt >"$tmp/chain" && install -m 0644 "$tmp/chain" "$name.chain.crt"
	printf '%s\n' "$san" >"$name.san"
	echo "made cert $name"
fi
echo "pin $(cat ca.sha256)"
