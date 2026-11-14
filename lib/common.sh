# common.sh — constants and helpers of the operator tools (bin/hub-*, lib/mint). No side effects on source.
# The daemon has its own copy of the role table and these limits; go/hub's TestTablesMatchCommonSh holds them
# to one another.

: "${HUB_ROOT:=/var/lib/hub}"

CLAIMS="$HUB_ROOT/claims"          # claim registry root
INBOX="$HUB_ROOT/inbox"            # message spool: inbox/<recipient>/<msg_id>.msg (+ <msg_id>/ folder)
ALIASES="$HUB_ROOT/aliases"        # backend endpoint registry: aliases/<alias> -> type/addr
BUDGETS="$HUB_ROOT/budgets"        # operator-set flood budgets: budgets/<role> or budgets/<identity>
BYFPR="$CLAIMS/.byfpr"             # keyid -> claim symlink index
IBYFPR="$CLAIMS/.ibyfpr"           # the same for instance keys (docs/identities.md, Instances)
TMPD="$CLAIMS/.tmp"                # staging for atomic claim writes
SERIALD="$CLAIMS/.serial"          # persistent per-name generation counter
JOURNAL="$CLAIMS/.journal"         # append-only audit log
DESCRIBED="$HUB_ROOT/described"    # described/<identity>: registered descriptions (docs/help.md)
TOPICS="$HUB_ROOT/topics"          # topics/<topic>/{policy,subs/}   (docs/topics.md)
SUBS="$HUB_ROOT/subs"              # subs/<sub> -> ../topics/<topic>/subs/<sub>

MSG_EXT="msg"                      # message filename extension; msg_id is the basename

# Flood control defaults (per sender->recipient), in integer millitokens. hub-budget shows them.
FLOOD_RATE_MT=500                  # refill 500 millitokens/sec = 1 message / 2s sustained
FLOOD_BURST_MT=12000               # bucket cap = 12 messages burst
TOPIC_RATE=10                      # default publish rate per topic, messages/sec
TOPIC_BURST=100                    # default publish burst per topic, messages

OP_TTL_SECS=$((10*365*24*3600))    # hub-mint default lifetime: operator-minted roles are permanent
OP_MINTER='operator'               # claims/<minter>/: a workspace id, or this for hub-mint
INST_MINTER='instance'             # ... or this for an instance its parent's holder registered

# Identity/name grammar (docs/identities.md):
#   <role>[.<project>][+<instance>][/<ws>]@<org>.ssh
# '+' introduces the instance; '/' pins the workspace; the domain is the Gitea org plus the
# fixed .ssh suffix, so an identity is also a legal e-mail address (Gitea rejects a
# single-label domain, and '+' in a domain). The role decides everything else — ROLES below.
ROLE_RE='^[a-z][a-z0-9-]{0,31}$'
PROJ_RE='^[a-z0-9_-][a-z0-9._-]{0,63}$'    # a repo name: Gitea permits dots; may start with - or _
INST_RE='^[a-z0-9][a-z0-9-]{0,31}$'
ORG_RE='^[a-z0-9][a-z0-9._-]{0,63}$'       # a Gitea org: dots permitted, '+' is not
WS_RE='^ws-[a-z0-9][a-z0-9-]{0,30}$'
ID_SUFFIX='.ssh'                           # fixed domain suffix carried by every identity

# The role table. A role not listed here is not an identity.
#   shape     pinned: carries /<ws>, one per org per workspace · wsless: never carries it
#   minter    workspace: that workspace's registrar, which appends /<ws> itself
#             operator:  hub-mint on the hub · hub: the hub's own address, never claimable
#   project, instance: req · opt · no — an operator role with instance `no` may take instances
#             (`hub-mint --instances`); a required instance is part of the name
#   in        what it may receive: yes · no · policy: from the publishers its policy allows (a topic)
#             · topic: only from its topic (a subscription)
#   out       whether it may send: yes · no
declare -gA ROLES=(
  #             shape  minter    project instance in     out
  [main]='      pinned operator  no      no       yes    yes'
  [run]='       pinned workspace opt     req      yes    yes'
  [foreman]='   wsless operator  no      no       yes    yes'
  [frontline]=' wsless operator  no      no       yes    yes'
  [vmmanager]=' pinned operator  no      no       yes    yes'
  [inference]=' pinned operator  no      no       yes    yes'
  [group]='     wsless operator  req     no       yes    yes'
  [svc]='       wsless operator  req     no       yes    yes'   # a service: a group's capabilities, its own name
  [commhub]='   wsless operator  no      no       yes    yes'
  [publisher]=' wsless operator  no      req      no     yes'
  [topic]='     wsless hub       opt     req      policy no'
  [sub]='       wsless hub       req     req      topic  no'
)
ALIAS_RE='^[a-z][a-z0-9._-]{0,63}$'
# backend plugin types
BACKEND_TYPES='email tg'

valid_ws(){ [[ ${1:-} =~ $WS_RE ]]; }

# role_row <role> — sets ROLE_SHAPE ROLE_MINTER ROLE_PROJ ROLE_INST ROLE_IN ROLE_OUT; nonzero for an unknown role
role_row(){
  [[ ${1:-} =~ $ROLE_RE ]] && [ -n "${ROLES[$1]-}" ] || return 1
  read -r ROLE_SHAPE ROLE_MINTER ROLE_PROJ ROLE_INST ROLE_IN ROLE_OUT <<< "${ROLES[$1]}"
}
role_get(){ # <role> <shape|minter|project|instance|in|out>
  role_row "${1:-}" || return 1
  case "$2" in shape) printf '%s' "$ROLE_SHAPE";; minter) printf '%s' "$ROLE_MINTER";;
               project) printf '%s' "$ROLE_PROJ";; instance) printf '%s' "$ROLE_INST";;
               in) printf '%s' "$ROLE_IN";; out) printf '%s' "$ROLE_OUT";; *) return 1;; esac
}
role_known(){ role_row "${1:-}"; }
slot_ok(){ case "$1" in req) [ -n "$2" ];; no) [ -z "$2" ];; opt) return 0;; *) return 1;; esac; }

# The role of a name is up to the first '.' or '+'.
name_role(){ local h="${1%%+*}"; printf '%s' "${h%%.*}"; }

# name = <role>[.<project>][+<instance>], each part checked against its grammar
valid_name(){
  local n="${1:-}" h role proj="" inst=""
  [ -n "$n" ] || return 1
  h="${n%%+*}"; role="${h%%.*}"
  case "$h" in *.) return 1;; *.*) proj="${h#*.}";; esac      # a '.' needs a project after it
  case "$n" in *+) return 1;; *+*) inst="${n#*+}";; esac      # a '+' needs an instance after it
  [[ $role =~ $ROLE_RE ]] || return 1
  [ -z "$proj" ] || { [[ $proj =~ $PROJ_RE ]] && [[ $proj != *..* ]]; } || return 1
  [ -z "$inst" ] || [[ $inst =~ $INST_RE ]]
}

valid_org(){ [[ ${1:-} =~ $ORG_RE ]] && [[ ${1:-} != *..* ]]; }

# Split an identity into name / org / ws.  ws is empty for a ws-less identity.
# local part = <role>[.<project>][+<instance>][/<ws>] ; domain = <org>.ssh
id_name(){ local l="${1%%@*}"; printf '%s' "${l%%/*}"; }
id_org(){  local d="${1#*@}"; printf '%s' "${d%"$ID_SUFFIX"}"; }
id_role(){ name_role "$(id_name "$1")"; }
# id_split <identity> — the same parts: sets ID_NAME ID_ROLE ID_PROJ ID_INST ID_WS ID_ORG
id_split(){
  local l="${1%%@*}" h
  ID_NAME="${l%%/*}"; ID_ORG="${1#*@}"; ID_ORG="${ID_ORG%"$ID_SUFFIX"}"
  case "$l" in */*) ID_WS="${l#*/}";; *) ID_WS="";; esac
  h="${ID_NAME%%+*}"; ID_ROLE="${h%%.*}"
  case "$h" in *.*) ID_PROJ="${h#*.}";; *) ID_PROJ="";; esac
  case "$ID_NAME" in *+*) ID_INST="${ID_NAME#*+}";; *) ID_INST="";; esac
}

# Every identity's domain must carry the fixed suffix; org is what precedes it.
valid_suffix(){ local d="${1#*@}"; [ "$d" != "${d%"$ID_SUFFIX"}" ]; }

# A complete identity: the grammar, a known role, the role's slots, and its shape — a pinned
# role always carries /<ws>, a ws-less one never does. Prints nothing; see ident_problem.
valid_ident(){ [ -z "$(ident_problem "${1:-}")" ]; }

# Why an identity is not valid, for messages; empty when it is.
ident_problem(){
  local i="${1:-}"
  [[ $i == *@* ]] && valid_suffix "$i" || { printf 'need <name>@<org>%s' "$ID_SUFFIX"; return; }
  id_split "$i"
  valid_org "$ID_ORG" || { printf 'bad org'; return; }
  valid_name "$ID_NAME" && [[ $ID_WS != */* ]] || { printf 'bad name'; return; }
  role_row "$ID_ROLE" || { printf 'unknown role %s' "$ID_ROLE"; return; }
  slot_ok "$ROLE_PROJ" "$ID_PROJ" || { printf 'role %s: project is %s' "$ID_ROLE" "$(slot_word "$ROLE_PROJ")"; return; }
  # an operator role's identity with +<instance> is an instance form: whether its address takes instances
  # is the address's claim's to say, not the grammar's
  slot_ok "$ROLE_INST" "$ID_INST" || { [ "$ROLE_INST" = no ] && [ "$ROLE_MINTER" = operator ]; } ||
    { printf 'role %s: instance is %s' "$ID_ROLE" "$(slot_word "$ROLE_INST")"; return; }
  case "$ROLE_SHAPE" in
    pinned) [ -n "$ID_WS" ] || { printf 'role %s is pinned: it carries /<ws>' "$ID_ROLE"; return; }
            valid_ws "$ID_WS" || printf 'bad workspace %s' "$ID_WS" ;;
    wsless) [ -z "$ID_WS" ] && [[ ${i%%@*} != */* ]] || printf 'role %s is ws-less: no /<ws>' "$ID_ROLE" ;;
  esac
}
slot_word(){ case "$1" in req) printf required;; no) printf 'not allowed';; *) printf optional;; esac; }

# Filesystem projection. An identity may contain '/', which is a path separator,
# so every on-disk name goes through this. '~' cannot occur in any identity part
# (see the *_RE above), so the mapping is unambiguous and reversible.
fs_name(){ local t='~'; printf '%s' "${1//\//$t}"; }   # $t: an unquoted ~ would tilde-expand
valid_alias(){ [[ ${1:-} =~ $ALIAS_RE ]]; }

# A claim lives under whoever minted it, filed by the identity itself:
#   CLAIMS/<minter>/<fq>      <minter> = the registering workspace, or $OP_MINTER
# The mailbox is always inbox/<fq>/ — the address is the name as written, and the
# minter is provenance in the record, never part of the destination.
claim_path(){ printf '%s/%s/%s' "$CLAIMS" "$2" "$(fs_name "$1")"; }
claim_find(){ # <fq> -> paths of every claim record for it, across minters
  shopt -s nullglob; local f
  for f in "$CLAIMS"/*/"$(fs_name "$1")"; do [ -f "$f" ] && printf '%s\n' "$f"; done; }
claim_live(){ # <record> -> 0 while its expiry is in the future
  local e; e="$(rec_get expires "$1")"
  [ -n "$e" ] && [ "$e" -gt "$(now_epoch)" ] 2>/dev/null; }
# Split brain: a live claim for <fq> under any minter but <minter>. Prints that minter.
claim_live_elsewhere(){ # <fq> <minter>
  local f d
  while IFS= read -r f; do
    d="${f%/*}"; d="${d##*/}"
    [ "$d" = "$2" ] && continue
    claim_live "$f" && { printf '%s' "$d"; return 0; }
  done < <(claim_find "$1")
  return 1
}
# One registry writer at a time: the cross-minter checks above are only meaningful if no
# other claim can land between them and the write. Holds fd 7 until the process exits.
claims_lock(){
  local l="$CLAIMS/.lock"
  [ -e "$l" ] || : >> "$l" 2>/dev/null
  exec 7<"$l" && flock -w 10 7
}
# The key index is 1:1, across both indexes. Prints the identity <keyid> is bound to, if not <rel> (its own
# record; every link is ../<minter>/<identity>, so one <rel> fits either index).
key_bound_elsewhere(){ # <keyid> <rel-link-target>
  local l
  for l in "$BYFPR/$1" "$IBYFPR/$1"; do
    [ -L "$l" ] && [ -e "$l" ] || continue         # unbound, or a dangling link left by a crash
    [ "$(readlink "$l")" = "$2" ] && continue
    printf '%s' "$(rec_get fq "$l")"; return 0
  done
  return 1
}

# --- instances (docs/identities.md, Instances) -------------------------------------------
# inst_split <identity> — 0 for an instance form (an operator role's identity with +<instance>, the role
# having no instance slot of its own): sets INST_PARENT (the address it is an instance of) and INST_ID.
# Overwrites the ID_* of id_split. A publisher's +<app> is its name, not an instance.
inst_split(){
  id_split "$1"
  [ -n "$ID_INST" ] && role_row "$ID_ROLE" && [ "$ROLE_MINTER" = operator ] && [ "$ROLE_INST" = no ] || return 1
  INST_ID="$ID_INST"; INST_PARENT="${ID_NAME%%+*}${ID_WS:+/$ID_WS}@$ID_ORG$ID_SUFFIX"
}
# drop an instance's claim and key link; the daemon observes that: its follow ends, its sessions are refused
# from their next command
inst_drop(){ # <instance record>
  local k; k="$(rec_get keyid "$1")"
  [ "$(readlink "$IBYFPR/$k" 2>/dev/null)" = "../$INST_MINTER/${1##*/}" ] && rm -f "$IBYFPR/$k"
  rm -f "$1"
}
evict_instances(){ # <address> — every instance its holder registered
  local r n=0; shopt -s nullglob
  for r in "$CLAIMS/$INST_MINTER"/*; do
    [ -f "$r" ] && [ "$(rec_get parent "$r")" = "$1" ] || continue
    inst_drop "$r"; n=$((n+1))
  done
  [ "$n" = 0 ] || journal "evict $n instances of $1"
}

# Which orgs a workspace was enrolled for. One org per line; absent file = none.
ws_may_claim_org(){
  local f="$HUB_ROOT/orgs/$1"
  [ -r "$f" ] || return 1
  grep -qxF -- "$2" "$f"
}

keyid_from_blob(){ printf '%s' "$1" | sha256sum | cut -d' ' -f1; }
ssh_fpr(){ printf '%s %s\n' "$1" "$2" | ssh-keygen -lf - 2>/dev/null | awk '{print $2}'; }

now_epoch(){ date -u +%s; }
iso_time(){ date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

rec_get(){ # <key> <file>  (always succeeds; empty if absent — safe under set -e)
  local k v; [ -f "$2" ] || return 0
  while IFS='=' read -r k v; do [ "$k" = "$1" ] && { printf '%s' "$v"; return 0; }; done < "$2"
  return 0
}

journal(){ { printf '%s %s\n' "$(iso_time "$(now_epoch)")" "$*" >> "$JOURNAL"; } 2>/dev/null || true; }   # best effort, silent

# --- topics and subscriptions (docs/topics.md) -----------------------------------------
topic_dir(){ printf '%s/%s' "$TOPICS" "$(fs_name "$1")"; }
sub_link(){  printf '%s/%s' "$SUBS" "$(fs_name "$1")"; }

# --- flood budgets (bin/hub-budget) ---------------------------------------------------
# rate_mt <msgs/s> — millitokens per second for a decimal rate with up to 3 decimals
rate_mt(){
  [[ ${1:-} =~ ^([0-9]{1,6})(\.([0-9]{1,3}))?$ ]] || return 1
  local i="${BASH_REMATCH[1]}" f="${BASH_REMATCH[3]}000" v
  v=$(( 10#$i * 1000 + 10#${f:0:3} )); [ "$v" -gt 0 ] || return 1
  printf '%s' "$v"
}
mt_rate(){ # <millitokens/s> -> the decimal rate
  local i=$(( $1 / 1000 )) f; f="$(printf '%03d' $(( $1 % 1000 )))"
  while [[ $f == *0 ]]; do f="${f%0}"; done
  printf '%s%s' "$i" "${f:+.$f}"
}
