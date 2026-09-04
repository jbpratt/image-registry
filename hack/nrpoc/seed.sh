#!/usr/bin/env bash
# SC2016: the push "$0" "$1" call is meant to expand in the child bash, not here.
# shellcheck disable=SC2016
# Seed one registry namespace with REPOS repos x TAGS tags of a tiny image.
# Usage: seed.sh <ns> <token-file> [repos] [tags] [parallel]
# Env: REGISTRY (default 127.0.0.1:5000), OCI (oci image layout dir)
set -euo pipefail
ns=$1; tokenfile=$2; repos=${3:-100}; tags=${4:-5}; par=${5:-8}
registry=${REGISTRY:-127.0.0.1:5000}
oci=${OCI:?set OCI to the oci: layout dir}
token=$(cat "$tokenfile")

push() {
  local repo=$1 tag=$2
  for _ in 1 2 3 4 5; do
    if skopeo copy --quiet --dest-tls-verify=false --dest-creds="pusher:$token" \
      "oci:${oci}:latest" "docker://${registry}/${ns}/${repo}:${tag}"; then
      return 0
    fi
    sleep 1
  done
  echo "FAILED ${repo}:${tag}" >&2
  return 1
}
export -f push
export registry ns token oci

start=$(date +%s)
for r in $(seq -w 0 $((repos-1))); do
  for t in $(seq 0 $((tags-1))); do
    echo "repo${r} tag${t}"
  done
done | xargs -P "$par" -n2 bash -c 'push "$0" "$1"'
end=$(date +%s)
echo "seeded ${ns}: ${repos}x${tags} in $((end-start))s"
