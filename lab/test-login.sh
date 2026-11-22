#!/usr/bin/env bash
# lab/test-login.sh <lab> [<results file>]: login on the lab's Holdfast server (decision 034, plan-m4 Q9), through
# Authelia on the same VM, from this host as a person's browser goes but with curl. lab/test-fidelity.sh runs it, after
# lab/lab test has provisioned the server, and its summary has these checks:
#   1. Authelia's discovery document, <issuer>/.well-known/openid-configuration, has the issuer that server.yaml names
#      and lists the device_authorization_endpoint, which holdfast login needs
#   2. GET <ui>/api/v1/me without a session answers 401
#   3. GET <ui>/auth/login answers 302 to Authelia's authorization endpoint, with PKCE (S256) and the redirect URI
#      <ui>/auth/callback
#   4. ada logs in without a browser: Authelia's first factor (POST <issuer>/api/firstfactor, her password from
#      credentials/authelia-ada.pw on curl's stdin), then that authorization request again, which Authelia now answers
#      with a redirect to <ui>/auth/callback; the callback sets Holdfast's session cookie, and GET <ui>/api/v1/me with it
#      is ada, admin everywhere (holdfast-admins' default binding)
#   5. POST <ui>/auth/logout ends that session: GET <ui>/api/v1/me answers 401 again
# curl reaches the server at its address (--resolve) and checks every certificate against the lab's CA. Each check's
# line goes to the log and the terminal, and to <results file>: PASS or FAIL, a tab, the check, a tab, what was seen.
# Exit 0 when every check passes, 1 when one does not, 2 misused, 3 the lab is not up. The password, the cookies and the
# authorization code are never in an argument, the log or the terminal; the cookies are removed at the end.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
results=${2:-/dev/null}
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "test-login: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
[[ -n ${HF_UI_URL:-} && -n ${HF_OIDC_ISSUER:-} ]] ||
	{ echo "test-login: $state/lab.env has no HF_UI_URL or HF_OIDC_ISSUER (lab/lab test $lab writes it again)" >&2; exit 3; }
user=ada
ca=$HF_STATE/ca/ca.crt
pw=$HF_STATE/credentials/authelia-$user.pw
[[ -s $ca && -s $pw ]] || { echo "test-login: no $ca or $pw: the server is not provisioned (lab/lab test $lab does it)" >&2; exit 3; }
ip=HF_VM_IP_$HF_SERVER_VM
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/jar"
: >"$results"
{ true >&3; } 2>/dev/null || exec 3>&1
say() { echo "== $*"; echo "lab: $*" >&3; }

# Both names are the server's: curl goes to its address for each host:port, without this host's /etc/hosts.
resolve=()
for u in "$HF_UI_URL" "$HF_OIDC_ISSUER"; do
	hp=${u#https://} && hp=${hp%%/*}
	[[ $hp == *:* ]] || hp+=:443
	resolve+=(--resolve "$hp:${!ip}")
done
# hc <curl arguments>: one request from this host, with the cookie jar, nothing followed.
hc() { curl -sS -m 30 --cacert "$ca" "${resolve[@]}" -b "$work/jar" -c "$work/jar" "$@"; }
# bare <url>: a URL without its query, for the log (the query has the state, and the callback's the code).
bare() { printf '%s' "${1%%\?*}"; }
js() { python3 -I -c "import json, sys
j = json.load(sys.stdin)
$1" "${@:2}"; }

passed=0 total=0
check() {
	local ok=$1 name=$2 seen=$3
	total=$((total + 1))
	[[ $ok == PASS ]] && passed=$((passed + 1))
	printf '%s\t%s\t%s\n' "$ok" "$name" "$seen" >>"$results"
	echo "$ok  $name: $seen"
	say "  login: $ok: $name ($seen)"
}

say "login on the UI $HF_UI_URL through Authelia $HF_AUTHELIA_VERSION at $HF_OIDC_ISSUER, from this host, as $user"

# 1. The provider's discovery document.
authz=
if out=$(hc -f "$HF_OIDC_ISSUER/.well-known/openid-configuration" 2>&1); then
	read -r iss authz device <<<"$(js 'print(j.get("issuer") or "-", j.get("authorization_endpoint") or "-", j.get("device_authorization_endpoint") or "-")' <<<"$out")"
	if [[ $iss == "$HF_OIDC_ISSUER" && $device == https://* && $authz == https://* ]]; then
		check PASS "Authelia's discovery lists the device_authorization_endpoint" "issuer $iss, device flow at $device"
	else
		check FAIL "Authelia's discovery lists the device_authorization_endpoint" "issuer $iss (server.yaml has $HF_OIDC_ISSUER), device_authorization_endpoint $device"
	fi
else
	check FAIL "Authelia's discovery lists the device_authorization_endpoint" "GET $HF_OIDC_ISSUER/.well-known/openid-configuration: $out"
fi

# 2. No session, no API.
code=$(hc -o /dev/null -w '%{http_code}' "$HF_UI_URL/api/v1/me" 2>&1) || true
if [[ $code == 401 ]]; then
	check PASS "GET /api/v1/me without a session answers 401" "401"
else
	check FAIL "GET /api/v1/me without a session answers 401" "$code"
fi

# 3. The UI's login sends the browser to the provider, with PKCE and the callback.
login=
read -r code login <<<"$(hc -o /dev/null -w '%{http_code} %{redirect_url}' "$HF_UI_URL/auth/login" 2>&1)" || true
want_redirect=redirect_uri=$(python3 -I -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$HF_UI_URL/auth/callback")
if [[ $code == 302 && -n $authz && $login == "$authz?"* && $login == *code_challenge_method=S256* && $login == *"$want_redirect"* ]]; then
	check PASS "GET /auth/login redirects to Authelia's authorization endpoint" "302 to $(bare "$login"), PKCE S256, redirect_uri $HF_UI_URL/auth/callback"
else
	check FAIL "GET /auth/login redirects to Authelia's authorization endpoint" "$code to $(bare "${login:-nothing}") (want $authz with code_challenge_method=S256 and $want_redirect)"
	login=
fi

# 4. ada logs in: the first factor at Authelia, then the same authorization request, Holdfast's callback, and her session.
name="$user logs in through Authelia: /api/v1/me is $user, admin everywhere"
seen=
if [[ -z $login ]]; then
	seen="no authorization request to follow (check 3)"
else
	code=$(python3 -I -c 'import json, sys; print(json.dumps({"username": sys.argv[1], "password": open(sys.argv[2]).read().strip(), "keepMeLoggedIn": False}))' "$user" "$pw" |
		hc -o "$work/firstfactor" -w '%{http_code}' -H 'Content-Type: application/json' --data-binary @- "$HF_OIDC_ISSUER/api/firstfactor" 2>&1) || true
	status=$(js 'print(j.get("status", "-"))' <"$work/firstfactor" 2>/dev/null || echo -)
	echo "POST $HF_OIDC_ISSUER/api/firstfactor as $user: $code, status $status"
	if [[ $code != 200 || $status != OK ]]; then
		seen="POST /api/firstfactor answered $code, status $status"
	else
		read -r code cb <<<"$(hc -o /dev/null -w '%{http_code} %{redirect_url}' "$login" 2>&1)" || true
		echo "GET $(bare "$login") again, with Authelia's session: $code to $(bare "${cb:-nothing}")"
		if [[ ($code != 302 && $code != 303) || $cb != "$HF_UI_URL/auth/callback?"* ]]; then
			seen="the authorization request answered $code to $(bare "${cb:-nothing}"), not to $HF_UI_URL/auth/callback"
		else
			code=$(hc -o "$work/callback" -w '%{http_code}' "$cb" 2>&1) || true
			echo "GET $(bare "$cb"): $code"
			if [[ $code != 200 ]] || ! grep -q $'\tholdfast_session\t' "$work/jar"; then
				seen="the callback answered $code$( [[ $code == 200 ]] || printf ': %s' "$(head -c 300 "$work/callback")"), session cookie: $(grep -c $'\tholdfast_session\t' "$work/jar" || true)"
			else
				code=$(hc -o "$work/me" -w '%{http_code}' "$HF_UI_URL/api/v1/me" 2>&1) || true
				me=$(js 'print(j.get("name", "-"), str(j.get("admin_everywhere")).lower(), ",".join(j.get("groups") or []) or "-", j.get("source", "-"))' <"$work/me" 2>/dev/null || echo "- - - -")
				read -r who admin groups source <<<"$me"
				echo "GET $HF_UI_URL/api/v1/me with Holdfast's session: $code, name $who, admin_everywhere $admin, groups $groups, source $source"
				seen="/api/v1/me: $code, name $who, admin_everywhere $admin, groups $groups"
				if [[ $code == 200 && $who == "$user" && $admin == true ]]; then
					check PASS "$name" "$seen"
					seen=ok
				fi
			fi
		fi
	fi
fi
[[ $seen == ok ]] || check FAIL "$name" "$seen"

# 5. Logout ends the session (a browser's POST comes from the UI's own origin).
name="POST /auth/logout ends the session: /api/v1/me 401 again"
if [[ $seen == ok ]]; then
	origin=${HF_UI_URL%%/}
	code=$(hc -o /dev/null -w '%{http_code}' -X POST -H "Origin: $origin" "$HF_UI_URL/auth/logout" 2>&1) || true
	after=$(hc -o /dev/null -w '%{http_code}' "$HF_UI_URL/api/v1/me" 2>&1) || true
	if [[ $code == 204 && $after == 401 ]]; then
		check PASS "$name" "logout $code, then /api/v1/me $after"
	else
		check FAIL "$name" "logout $code, then /api/v1/me $after"
	fi
else
	check FAIL "$name" "no session to end (check 4)"
fi

say "  login: $passed of $total checks passed"
((passed == total))
