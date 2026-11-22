#!/usr/bin/env bash
# authelia-credentials.sh <authelia> <credentials dir> <user>...: the secrets of the lab's Authelia, made once each in the
# lab's credentials (every file 0600) and never changed afterwards. lab/ansible/site.yml runs it on this host, with the
# authelia binary of the pinned release.
#   authelia-<user>.pw, .digest       each user's password (32 letters and digits) and its argon2id digest, which
#                                     Authelia's user database holds; made together by `authelia crypto hash generate
#                                     argon2 --random`, so no password is ever in an argument
#   authelia-client.secret, .digest   Holdfast's client secret (server.yaml's auth.oidc.client_secret_file) and its
#                                     pbkdf2-sha512 digest, which Authelia's client registration holds
#   authelia-jwks.pem                 the RSA key Authelia signs ID tokens with
#   authelia-jwt.secret, authelia-session.secret, authelia-storage.secret, authelia-hmac.secret
#                                     identity_validation.reset_password.jwt_secret, session.secret,
#                                     storage.encryption_key and identity_providers.oidc.hmac_secret (64 letters and digits)
# It prints "made <file>" for each file it makes, and nothing else. No secret is ever printed.
set -euo pipefail
au=$1 dir=$2
shift 2
umask 077
[[ -x $au ]] || { echo "authelia-credentials.sh: no $au" >&2; exit 2; }
[[ -d $dir ]] || { echo "authelia-credentials.sh: no directory $dir" >&2; exit 2; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# authelia reads configuration.yml from its working directory when there is one: there is none in $tmp.
cd "$tmp"

# pair <name> <secret extension> <hash algorithm and its flags>: a random secret and its digest, both made anew when
# either is missing, so the two always match.
pair() {
	local name=$1 ext=$2
	shift 2
	[[ -s $dir/$name.$ext && -s $dir/$name.digest ]] && return 0
	"$au" crypto hash generate "$@" --random --random.length 32 --random.charset alphanumeric >"$tmp/out"
	sed -n 's/^Random Password: //p' "$tmp/out" >"$tmp/secret"
	sed -n 's/^Digest: //p' "$tmp/out" >"$tmp/digest"
	if [[ $(wc -l <"$tmp/secret") != 1 || $(wc -l <"$tmp/digest") != 1 || ! -s $tmp/secret || ! -s $tmp/digest ]]; then
		echo "authelia-credentials.sh: authelia crypto hash generate $1 did not print one password and one digest" >&2
		exit 1
	fi
	rm -f "$tmp/out"
	mv "$tmp/digest" "$dir/$name.digest"
	mv "$tmp/secret" "$dir/$name.$ext"
	echo "made $name.$ext"
	echo "made $name.digest"
}

for user in "$@"; do
	[[ $user =~ ^[a-z][a-z0-9]*$ ]] || { echo "authelia-credentials.sh: '$user' is not a user name" >&2; exit 2; }
	pair "authelia-$user" pw argon2
done
pair authelia-client secret pbkdf2 --variant sha512

if [[ ! -s $dir/authelia-jwks.pem ]]; then
	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$tmp/jwks.pem" 2>/dev/null
	mv "$tmp/jwks.pem" "$dir/authelia-jwks.pem"
	echo "made authelia-jwks.pem"
fi

for s in jwt session storage hmac; do
	[[ -s $dir/authelia-$s.secret ]] && continue
	"$au" crypto rand --length 64 --charset alphanumeric --file "$tmp/$s" >/dev/null
	[[ -s $tmp/$s ]] || { echo "authelia-credentials.sh: authelia crypto rand made no $s secret" >&2; exit 1; }
	mv "$tmp/$s" "$dir/authelia-$s.secret"
	echo "made authelia-$s.secret"
done
