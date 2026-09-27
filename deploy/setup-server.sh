#!/bin/sh
# Sets up an Ubuntu server (24.04 or later) to run the AIShie Agent Runtime,
# beside AIShiteru Core (a server set up by Core's deploy/setup-server.sh) or
# on its own. Run as root with this directory copied to the server
# (docs/deploying.md):
#
#   sh deploy/setup-server.sh lms-staging.example.edu staging [https://lms-staging.example.edu]
#
# The name is this server's, as the Deploy workflow reaches it over SSH. The
# environment, staging or production, names the GitHub settings it prints.
# The URL is the Core the agents here may connect to
# (CORE_BASE_URL_ALLOWLIST); on a server that runs Core it defaults to Core's
# PUBLIC_URL.
#
# It installs Docker and PostgreSQL where they are missing; creates the
# runtime's own database, never Core's, and the env file with a generated
# database password; the directories for the agents' configuration and
# secrets, the backup directory and a nightly backup; installs
# aishie-runtime-deploy and aishie-runtime; allows SSH in ufw when ufw is on;
# and makes an SSH user, aishie-deploy, that can do one thing: run
# aishie-runtime-deploy, for this repository's Deploy workflow. Core's deploy
# user and its key are another user and another key: each repository deploys
# only its own image. The runtime serves nothing to the internet: /healthz,
# /status and /metrics are on 127.0.0.1:9090.
#
# Run again, it installs the scripts in this directory over the old ones and
# leaves everything else as it is: the env file, the database, the
# configuration, the secrets and aishie-deploy's key. That is how a newer
# aishie-runtime-deploy reaches the server.
set -eu

HOST=${1:-}
ENVIRONMENT=${2:-}
CORE_URL=${3:-}
usage() {
  echo "usage: setup-server.sh HOSTNAME staging|production [CORE_URL], e.g. lms-staging.example.edu staging https://lms-staging.example.edu" >&2
  exit 2
}
case $HOST in '' | *[!A-Za-z0-9.-]* | .* | -*) usage ;; esac
# It names the GitHub settings this server needs; guessing would name the
# other environment's.
case $ENVIRONMENT in staging | production) ;; *) usage ;; esac
[ "$(id -u)" = 0 ] || { echo "run this as root (sudo -i)" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
ETC=/etc/aishie-runtime
ENV_FILE=$ETC/runtime.env
CORE_ENV_FILE=/etc/aishiteru/aishiteru.env
BACKUPS=/var/backups/aishie-runtime
KEY=/root/aishie-runtime-deploy-key
USER_NAME=aishie-deploy
# The runtime's database and its role: never Core's (aishiteru).
DB=aishie_runtime
say() { printf '\n== %s\n' "$*"; }
systemd() { [ -d /run/systemd/system ]; }

# The Core the agents may connect to: an origin, https (http only on this
# machine), with no path. Only a new env file needs it.
if [ -z "$CORE_URL" ] && [ -r "$CORE_ENV_FILE" ]; then
  CORE_URL=$(sed -n 's/^PUBLIC_URL=//p' "$CORE_ENV_FILE" | tail -n 1)
  [ -z "$CORE_URL" ] || echo "Core on this server is $CORE_URL ($CORE_ENV_FILE)"
fi
bad_url() { echo "the Core's URL is an origin, like https://lms-staging.example.edu: https, no path, no / at the end (not $CORE_URL)" >&2; exit 2; }
if [ -n "$CORE_URL" ]; then
  case $CORE_URL in https://* | http://localhost:* | http://127.0.0.1:*) ;; *) bad_url ;; esac
  case ${CORE_URL#*://} in '' | */* | *[!A-Za-z0-9.:-]*) bad_url ;; esac
elif [ ! -e "$ENV_FILE" ]; then
  echo "name the Core the agents here connect to, e.g. https://lms-staging.example.edu, after the environment" >&2
  usage
fi

say "Packages"
# A server that has just booted is often still updating itself, and apt-get,
# unlike apt, does not wait for the lock.
if command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait >/dev/null 2>&1 || true; fi
pkgs="postgresql openssh-server cron curl"
command -v docker >/dev/null 2>&1 || pkgs="docker.io $pkgs"
i=0
until apt-get update -q; do
  i=$((i + 1))
  [ "$i" -lt 20 ] || { echo "apt-get update kept failing: run this again in a while" >&2; exit 1; }
  echo "apt is busy, most likely the server updating itself: trying again in 30 seconds"
  sleep 30
done
# shellcheck disable=SC2086 # a list of package names
DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y -q $pkgs
if systemd; then systemctl enable --now docker postgresql ssh cron; fi

say "Database $DB and $ENV_FILE"
role=$(runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = '$DB'")
if [ -e "$ENV_FILE" ]; then
  echo "$ENV_FILE is there already: left as it is"
  [ "$role" = 1 ] || echo "warning: there is no database role $DB; $ENV_FILE may be for another database" >&2
elif [ "$role" = 1 ]; then
  cat >&2 <<MSG
The database role $DB exists, but there is no
$ENV_FILE: an earlier run stopped half way. If this
server holds no runtime data yet, remove them and run this again:

  runuser -u postgres -- dropdb --if-exists $DB
  runuser -u postgres -- dropuser $DB

Otherwise give the role a new password (openssl rand -hex 24), with psql:
ALTER ROLE $DB PASSWORD '...'; and write the env file, mode 600,
with the lines this script writes: DATABASE_URL, HTTP_ADDR,
CORE_BASE_URL_ALLOWLIST and LOG_FORMAT (see the script).
MSG
  exit 1
else
  # Hex only, so that neither the SQL nor the URL needs anything escaped.
  pw=$(openssl rand -hex 24)
  install -d -m 700 "$ETC"
  (umask 077 && cat > "$ENV_FILE.new" <<ENVEOF)
DATABASE_URL=postgres://$DB:$pw@127.0.0.1:5432/$DB
HTTP_ADDR=127.0.0.1:9090
CORE_BASE_URL_ALLOWLIST=$CORE_URL
LOG_FORMAT=json
ENVEOF
  # On psql's input, not its command line, where anyone could read the
  # password; and kept out of the server's log should the statement fail.
  printf "SET log_min_error_statement = panic;\nCREATE ROLE %s LOGIN PASSWORD '%s';\nCREATE DATABASE %s OWNER %s;\n" "$DB" "$pw" "$DB" "$DB" |
    runuser -u postgres -- psql -q -v ON_ERROR_STOP=1
  mv "$ENV_FILE.new" "$ENV_FILE"
  echo "wrote $ENV_FILE"
fi

say "Directories, scripts and the nightly backup"
install -d -m 700 "$ETC"
# The runtime runs as user 65532 (distroless nonroot), which reads these
# through their group; root owns them and writes them.
for d in "$ETC/agents" "$ETC/secrets"; do
  if [ -d "$d" ]; then echo "$d is there already: left as it is"; else install -d -o root -g 65532 -m 750 "$d" && echo "made $d"; fi
done
install -d -o postgres -g postgres -m 700 "$BACKUPS"
install -m 755 "$here/aishie-runtime-deploy" "$here/aishie-runtime" /usr/local/bin/
echo "installed aishie-runtime-deploy and aishie-runtime in /usr/local/bin"
cat > /etc/cron.d/aishie-runtime-backup <<CRONEOF
# AIShie Agent Runtime: a backup of its database every night, one per weekday
# (seven kept), half an hour after Core's.
# A job in /etc/cron.d gets no /usr/sbin, where runuser is, unless it says so.
PATH=/usr/sbin:/usr/bin:/sbin:/bin
30 3 * * * root f=$BACKUPS/daily-\$(date +\%u).dump; runuser -u postgres -- pg_dump -Fc -f "\$f.part" $DB && mv "\$f.part" "\$f"
CRONEOF

say "Firewall"
# Nothing of the runtime's is served from outside: only SSH, for the Deploy
# workflow.
if command -v ufw >/dev/null 2>&1 && ufw status | grep -q '^Status: active'; then
  ufw allow 22/tcp >/dev/null
  echo "ufw: 22 allowed"
else
  echo "ufw is off; a firewall of your provider's must allow 22"
fi
sshd_cfg=$(/usr/sbin/sshd -T 2>/dev/null || true)
if printf '%s\n' "$sshd_cfg" | grep -qx 'passwordauthentication yes'; then
  echo "warning: SSH takes passwords, and port 22 is open to the internet for the Deploy workflow." >&2
  echo "  Once you log in with a key: echo 'PasswordAuthentication no' > /etc/ssh/sshd_config.d/00-no-passwords.conf" >&2
  echo "  (sshd keeps the first value it reads, and cloud-init's 50-cloud-init.conf may say yes)," >&2
  echo "  systemctl restart ssh, and check that sshd -T | grep -i passwordauthentication says no" >&2
fi

say "SSH user $USER_NAME, for the Deploy workflow"
id "$USER_NAME" >/dev/null 2>&1 || useradd --create-home --shell /bin/sh "$USER_NAME"
# No password to log in with, and not locked either: a locked account is
# refused even with a key.
usermod -p '*' "$USER_NAME"
sudoers=/etc/sudoers.d/aishie-runtime-deploy
printf '%s ALL=(root) NOPASSWD: /usr/local/bin/aishie-runtime-deploy\n' "$USER_NAME" > "$sudoers.new"
chmod 440 "$sudoers.new"
visudo -cqf "$sudoers.new" && mv "$sudoers.new" "$sudoers"
home=$(getent passwd "$USER_NAME" | cut -d: -f6)
install -d -o "$USER_NAME" -g "$USER_NAME" -m 700 "$home/.ssh"
if grep -q 'aishie-runtime-deploy@' "$home/.ssh/authorized_keys" 2>/dev/null; then
  # The key in GitHub keeps working. To replace it: docs/deploying.md.
  echo "$USER_NAME's SSH key is set up already: left as it is"
else
  [ -e "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -C "aishie-runtime-deploy@$HOST" -f "$KEY"
  # The key can do nothing but this: no shell, no forwarding, and the command
  # it asks for is only ever the image aishie-runtime-deploy is given.
  # shellcheck disable=SC2016 # $SSH_ORIGINAL_COMMAND is for sshd to expand
  printf 'restrict,command="sudo -n /usr/local/bin/aishie-runtime-deploy \\"$SSH_ORIGINAL_COMMAND\\"" %s\n' "$(cat "$KEY.pub")" > "$home/.ssh/authorized_keys"
  chown "$USER_NAME:$USER_NAME" "$home/.ssh/authorized_keys"
  chmod 600 "$home/.ssh/authorized_keys"
  echo "made $USER_NAME's SSH key"
fi

upper=$(echo "$ENVIRONMENT" | tr '[:lower:]' '[:upper:]')
say "Done. What is left"
cat <<DONE
1. Let this server pull the image. Docker keeps one login per registry, so
   on a server that runs Core too, the account Core's image is pulled with
   must be able to read this one as well (a GitHub token, classic, with
   read:packages):
     docker login ghcr.io -u <GitHub user name>
2. The agents: their YAML in $ETC/agents, and each secret
   it refers to as a file in $ETC/secrets (secret://a/b is
   the file a/b there), readable by group 65532 (docs/deploying.md, The
   agents' configuration and secrets):
     install -g 65532 -m 640 tutor.yaml $ETC/agents/
     install -D -g 65532 -m 640 /dev/stdin $ETC/secrets/a/b    (paste, then Ctrl-D)
     AISHIE_RUNTIME_IMAGE=ghcr.io/aishiteru-lms/aishie-agent-runtime:sha-<commit> aishie-runtime check
3. Start it, with the image of the latest green push to main (the CI run's
   publish / image job, or the package's page, names it), or of a release:
     aishie-runtime-deploy ghcr.io/aishiteru-lms/aishie-agent-runtime:sha-<commit>
     curl -s 127.0.0.1:9090/healthz
4. For the Deploy workflow, in the AIShie-Agent-Runtime repository's
   Settings → Secrets and variables → Actions (not Core's):
     variable DEPLOY_TARGET_$upper       $USER_NAME@$HOST
     variable DEPLOY_KNOWN_HOSTS_$upper  $HOST $(cut -d' ' -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)
DONE
if [ -e "$KEY" ]; then
  cat <<DONE
     secret   DEPLOY_SSH_KEY_$upper      the whole of $KEY (cat $KEY)
   then delete $KEY: the server keeps only its public half.
DONE
fi
