#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
env_file=${AI_USAGE_ENV_FILE:-"$repo_dir/.env.test"}
image=${AI_USAGE_IMAGE:-localhost/ai-usage-web:test}
volume=${AI_USAGE_VOLUME:-ai-usage-data}

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

podman build --tag "$image" "$repo_dir"
if ! podman volume exists "$volume"; then
  podman volume create "$volume" >/dev/null
fi
podman run --detach --replace \
  --name ai-usage-web \
  --env-file "$env_file" \
  --publish "$publish_ip:8080:8080" \
  --volume "$volume:/data" \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  "$image"

echo "Dashboard : http://$publish_ip:8080"
echo "État OAuth persistant : volume $volume"
