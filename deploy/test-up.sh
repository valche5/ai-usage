#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
env_file=${AI_USAGE_ENV_FILE:-"$repo_dir/.env.test"}
image=${AI_USAGE_IMAGE:-localhost/ai-usage-web:test}
data_dir=${AI_USAGE_DATA_DIR_HOST:-"$HOME/.local/share/ai-usage"}
old_volume=${AI_USAGE_VOLUME:-ai-usage-data}

if [ ! -r "$env_file" ]; then
  echo "fichier d'environnement introuvable : $env_file" >&2
  exit 1
fi

publish_ip=$(sed -n 's/^AI_USAGE_PUBLISH_IP=//p' "$env_file" | head -n 1)
case "$publish_ip" in
  "" | *[!0-9a-fA-F.:]*)
    echo "AI_USAGE_PUBLISH_IP est absent ou invalide dans $env_file" >&2
    exit 1
    ;;
esac

mkdir -p "$data_dir"
chmod 700 "$data_dir"

if [ ! -f "$data_dir/state.enc" ] && podman volume exists "$old_volume"; then
  src=$(podman volume inspect "$old_volume" --format '{{.Mountpoint}}')
  if [ -e "$src/state.enc" ]; then
    podman unshare cp -a "$src/state.enc" "$data_dir/state.enc"
    podman unshare chown 0:0 "$data_dir/state.enc"
    podman unshare chmod 600 "$data_dir/state.enc"
    echo "État migré depuis le volume $old_volume vers $data_dir"
  fi
fi

podman build --tag "$image" "$repo_dir"
podman run --detach --replace \
  --name ai-usage-web \
  --userns=keep-id \
  --user "$(id -u):$(id -g)" \
  --env-file "$env_file" \
  --publish "$publish_ip:8080:8080" \
  --volume "$data_dir:/data" \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  "$image"

echo "Dashboard : http://$publish_ip:8080"
echo "État OAuth persistant : $data_dir/state.enc"
