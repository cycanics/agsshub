#!/usr/bin/env bash
# setup.sh — make a running dev hub usable from a client: enroll a test workspace's registrar key,
# mint test identities, pin the hub's host key, write the client's ssh config. Re-runnable: keys
# already in --out are reused, so a second run renews what the first one minted.
#
#   dev-hub/setup.sh [--out DIR] [--ws WS] [--orgs ORG,...] [--host H] [--port P] [IDENTITY ...]
#
#   --out    client files (keys, ssh_config, known_hosts); default
#            ${XDG_STATE_HOME:-~/.local/state}/agsshub-dev-hub/<compose project>
#   --ws     the test workspace, enrolled with --orgs     (default ws-dev, home,expa)
#   --host, --port   where the client reaches the hub     (default: the published port)
#   IDENTITY operator roles are minted with hub-mint, `run` ones registered through the test
#            workspace's registrar      (default: commhub@home.ssh group.demo@expa.ssh)
#
# The compose project is COMPOSE_PROJECT_NAME, else the compose file's own name.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
COMPOSE=(docker compose -f "$DIR/compose.yaml")
OUT=""; WS=ws-dev; ORGS=home,expa; HOST=""; PORT=""; IDS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --out)  OUT="${2:?}";  shift 2 ;;
    --ws)   WS="${2:?}";   shift 2 ;;
    --orgs) ORGS="${2:?}"; shift 2 ;;
    --host) HOST="${2:?}"; shift 2 ;;
    --port) PORT="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    -*) echo "setup: unknown option $1" >&2; exit 2 ;;
    *)  IDS+=("$1"); shift ;;
  esac
done
[ ${#IDS[@]} -gt 0 ] || IDS=(commhub@home.ssh group.demo@expa.ssh)

C="$("${COMPOSE[@]}" ps -q hub 2>/dev/null || true)"
[ -n "$C" ] || { echo "setup: no dev hub running — docker compose -f $DIR/compose.yaml up -d --build --wait" >&2; exit 1; }
PROJECT="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$C")"
NETWORK="$(docker inspect -f '{{range $n, $_ := .NetworkSettings.Networks}}{{$n}} {{end}}' "$C" | awk '{print $1}')"
if [ -z "$HOST" ] || [ -z "$PORT" ]; then
  pub="$("${COMPOSE[@]}" port hub 2422 | head -1)"                  # e.g. 127.0.0.1:2422
  [ -n "$pub" ] || { echo "setup: the hub publishes no port; give --host and --port" >&2; exit 1; }
  if [ -z "$HOST" ]; then HOST="${pub%:*}"; case "$HOST" in 0.0.0.0|'[::]'|::) HOST=127.0.0.1 ;; esac; fi
  [ -n "$PORT" ] || PORT="${pub##*:}"
fi
: "${OUT:=${XDG_STATE_HOME:-$HOME/.local/state}/agsshub-dev-hub/$PROJECT}"
umask 077
mkdir -p "$OUT/keys"
OUT="$(cd "$OUT" && pwd)"
ALIAS_KEY=agsshub-dev-hub                      # HostKeyAlias: one known_hosts line for any address

fsn(){ local t='~'; printf '%s' "${1//\//$t}"; }
alias_of(){ local a="${1%.ssh}"; a="${a/@/.}"; printf '%s' "${a//\//.}"; }   # commhub@home.ssh -> commhub.home
key(){ local f="$OUT/keys/$(fsn "$1")"; [ -f "$f" ] || ssh-keygen -q -t ed25519 -N '' -C "$1" -f "$f"; printf '%s' "$f"; }
minter_of(){ docker exec "$C" bash -c '. /usr/local/lib/hub/common.sh; role_get "$(id_role "$1")" minter' _ "$1" || true; }

# write_config <file> <host> <port> [<identity> <key>]... — one Host per identity, plus the registrar
write_config(){
  local f="$1" h="$2" p="$3"; shift 3
  { echo "# agsshub dev hub, compose project $PROJECT — written by dev-hub/setup.sh"
    printf 'Host dev-hub-reg\n    User reg\n    IdentityFile "%s"\n' "$OUT/keys/registrar"
    while [ $# -ge 2 ]; do
      printf 'Host %s\n    User hub\n    IdentityFile "%s"\n' "$(alias_of "$1")" "$2"; shift 2
    done
    printf 'Host *\n    HostName %s\n    Port %s\n    HostKeyAlias %s\n    UserKnownHostsFile "%s"\n' "$h" "$p" "$ALIAS_KEY" "$OUT/known_hosts"
    printf '    StrictHostKeyChecking yes\n    IdentitiesOnly yes\n    BatchMode yes\n    ConnectTimeout 10\n    ServerAliveInterval 30\n'
  } > "$f"
}

# the hub's host key, under the alias every config uses
hk="$(docker exec "$C" cat /etc/ssh/ssh_host_ed25519_key.pub)"
printf '%s %s\n' "$ALIAS_KEY" "$(printf '%s' "$hk" | cut -d' ' -f1,2)" > "$OUT/known_hosts"

# the test workspace and its registrar key
reg="$(key registrar)"
docker exec -i "$C" hub-enroll "$WS" "$ORGS" < "$reg.pub" >&2
write_config "$OUT/ssh_config" "$HOST" "$PORT"

PAIRS=(); : > "$OUT/identities"
for id in "${IDS[@]}"; do
  k="$(key "$id")"
  case "$(minter_of "$id")" in
    operator)
      docker exec -i "$C" hub-mint add "$id" < "$k.pub" >&2
      fq="$id" ;;
    workspace)
      line="$(ssh -F "$OUT/ssh_config" dev-hub-reg register "$id" --ttl 86400 --mode persistent < "$k.pub")"
      echo "$line" >&2
      fq="${line#registered }"; fq="${fq%% *}" ;;
    *) echo "setup: $id is not an identity anyone can mint (docs/identities.md)" >&2; exit 2 ;;
  esac
  PAIRS+=("$fq" "$k")
  printf '%s\t%s\t%s\n' "$fq" "$(alias_of "$fq")" "$k" >> "$OUT/identities"
done
write_config "$OUT/ssh_config" "$HOST" "$PORT" "${PAIRS[@]}"
write_config "$OUT/ssh_config.net" "$ALIAS_KEY" 2422 "${PAIRS[@]}"
cat > "$OUT/env" <<EOF
DEV_HUB_HOST=$HOST
DEV_HUB_PORT=$PORT
DEV_HUB_NET_HOST=$ALIAS_KEY
DEV_HUB_NET_PORT=2422
DEV_HUB_NETWORK=$NETWORK
DEV_HUB_CONTAINER=$C
DEV_HUB_HOST_KEY_ALIAS=$ALIAS_KEY
DEV_HUB_KNOWN_HOSTS=$OUT/known_hosts
DEV_HUB_SSH_CONFIG=$OUT/ssh_config
DEV_HUB_REGISTRAR_KEY=$OUT/keys/registrar
DEV_HUB_WS=$WS
EOF

# every identity must now get in and know who it is
while IFS=$'\t' read -r fq al k; do
  got="$(ssh -F "$OUT/ssh_config" "$al" id)" || { echo "setup: $fq cannot log in" >&2; exit 1; }
  case "$got" in "$fq serial="*) ;; *) echo "setup: $fq logged in as '$got'" >&2; exit 1 ;; esac
done < "$OUT/identities"

first="$(head -1 "$OUT/identities" | cut -f2)"; firstk="$(head -1 "$OUT/identities" | cut -f3)"
cat <<EOF
dev hub        $HOST:$PORT   (compose project $PROJECT, container ${C:0:12}, network $NETWORK)
workspace      $WS, enrolled for $ORGS; registrar key $OUT/keys/registrar
client files   $OUT   (ssh_config, ssh_config.net, known_hosts, keys/, identities, env)

identity → ssh alias → key
$(while IFS=$'\t' read -r fq al k; do printf '  %-40s %-28s %s\n' "$fq" "$al" "$k"; done < "$OUT/identities")

with the config:
  ssh -F $OUT/ssh_config $first id
  ssh -F $OUT/ssh_config $first send <identity> -m:rfc5322 --id <msgid> < message.eml
  ssh -F $OUT/ssh_config $first recv
  ssh -F $OUT/ssh_config dev-hub-reg register run.<project>+<id>@<org>.ssh --ttl 3600 < key.pub

without it:
  ssh -p $PORT -i $firstk -o IdentitiesOnly=yes -o BatchMode=yes \\
      -o HostKeyAlias=$ALIAS_KEY -o UserKnownHostsFile=$OUT/known_hosts -o StrictHostKeyChecking=yes \\
      hub@$HOST id

from a container on network $NETWORK (mount $OUT at the same path):
  ssh -F $OUT/ssh_config.net $first id

operator, on the dev hub:
  docker exec -i ${C:0:12} hub-mint add <identity> < key.pub
  docker exec ${C:0:12} hub-budget set group --rate 20 --burst 200
EOF
