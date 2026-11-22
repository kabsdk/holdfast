#!/usr/bin/env bash
# test/e2e/authelia/setup.sh <state dir> <image>: a real OIDC provider for the e2e tests (plan-m4 Q9).
#
# Makes, once, in <state dir>: a test CA and a certificate for auth.holdfast.test and ui.holdfast.test, the
# signing key, the client secret, four users with their passwords, and Authelia's configuration; then starts
# Authelia on https://auth.holdfast.test:59091 (127.0.0.1, host network). The e2e container resolves both
# names to 127.0.0.1. Test data only: nothing here is a real secret.
#
# Users: ada (holdfast-admins), ed (holdfast-editors), vi (holdfast-viewers), wes (web-team, bound by the
# tests to one host). Their passwords are in <state dir>/users.env for the tests.
set -euo pipefail
S=$1
IMG=$2
NAME=holdfast-test-authelia
PORT=59091
UI=https://ui.holdfast.test:58443
au() { docker run --rm -u "$(id -u):$(id -g)" -v "$S:/out" "$IMG" authelia "$@"; }
rnd() { head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 48; }

if [[ ! -f $S/configuration.yml ]]; then
	mkdir -p "$S/ca" "$S/tls" "$S/jwks"
	au crypto certificate rsa generate --common-name "Holdfast e2e CA" --ca --directory /out/ca >/dev/null
	au crypto certificate rsa generate --common-name auth.holdfast.test --sans auth.holdfast.test,ui.holdfast.test \
		--path.ca /out/ca --directory /out/tls >/dev/null
	au crypto pair rsa generate --directory /out/jwks >/dev/null
	hash() { docker run --rm "$IMG" authelia crypto hash generate "$1" --password "$2" | sed -n 's/^Digest: //p'; }
	secret=$(rnd)
	printf '%s\n' "$secret" >"$S/client-secret"
	: >"$S/users.env"
	{
		echo "users:"
		for u in ada:holdfast-admins ed:holdfast-editors vi:holdfast-viewers wes:web-team; do
			name=${u%%:*} group=${u#*:} pw=$(rnd)
			echo "HOLDFAST_E2E_PASS_${name^^}=$pw" >>"$S/users.env"
			cat <<EOF
  $name:
    disabled: false
    displayname: '$name'
    email: '$name@holdfast.test'
    password: '$(hash argon2 "$pw")'
    groups: ['$group']
EOF
		done
	} >"$S/users.yml"
	client_hash=$(hash pbkdf2 "$secret")
	cat >"$S/configuration.yml" <<EOF
server:
  address: 'tcp://127.0.0.1:$PORT/'
  tls:
    key: '/config/tls/private.pem'
    certificate: '/config/tls/public.crt'
log:
  level: 'info'
identity_validation:
  reset_password:
    jwt_secret: '$(rnd)'
authentication_backend:
  file:
    path: '/config/users.yml'
access_control:
  default_policy: 'one_factor'
session:
  secret: '$(rnd)'
  cookies:
    - domain: 'holdfast.test'
      authelia_url: 'https://auth.holdfast.test:$PORT'
storage:
  encryption_key: '$(rnd)'
  local:
    path: '/config/db.sqlite3'
notifier:
  filesystem:
    filename: '/config/notification.txt'
identity_providers:
  oidc:
    hmac_secret: '$(rnd)'
    jwks:
      - key_id: 'e2e'
        algorithm: 'RS256'
        use: 'sig'
        key: {{ secret "/config/jwks/private.pem" | mindent 10 "|" | msquote }}
    clients:
      - client_id: 'holdfast'
        client_name: 'Holdfast e2e'
        client_secret: '$client_hash'
        public: false
        authorization_policy: 'one_factor'
        consent_mode: 'implicit'
        require_pkce: true
        pkce_challenge_method: 'S256'
        redirect_uris: ['$UI/auth/callback']
        scopes: ['openid', 'profile', 'groups', 'email']
        grant_types: ['authorization_code', 'urn:ietf:params:oauth:grant-type:device_code']
        response_types: ['code']
        token_endpoint_auth_method: 'client_secret_post'
EOF
fi

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network host -u "$(id -u):$(id -g)" -v "$S:/config" \
	-e X_AUTHELIA_CONFIG_FILTERS=template "$IMG" authelia --config /config/configuration.yml >/dev/null
for _ in $(seq 1 60); do
	if curl -fsk --resolve "auth.holdfast.test:$PORT:127.0.0.1" "https://auth.holdfast.test:$PORT/api/health" >/dev/null 2>&1; then
		exit 0
	fi
	sleep 1
done
echo "Authelia did not come up:" >&2
docker logs --tail 40 "$NAME" >&2
exit 1
