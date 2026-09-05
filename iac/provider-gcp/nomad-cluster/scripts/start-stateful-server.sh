#!/bin/bash
# Start the singleton control plane with Nomad and Consul data on the MIG's
# preserved state disk. The bind mounts keep the upstream run scripts and their
# default paths unchanged. Initial activation is an explicit fresh canary rebuild;
# this script does not claim to migrate state from a replaced boot disk.

set -euo pipefail

exec > >(tee /var/log/user-data.log | logger -t user-data -s 2>/dev/console) 2>&1

ulimit -n 65536
export GOMAXPROCS='nproc'

readonly state_device="/dev/disk/by-id/google-e2b-server-state"
readonly state_root="/mnt/e2b-server-state"
readonly consul_bootstrap_marker="$state_root/consul/.ashler-acl-bootstrapped"
readonly nomad_bootstrap_marker="$state_root/nomad/.ashler-acl-bootstrapped"
readonly allow_fresh_bootstrap="${ALLOW_FRESH_BOOTSTRAP}"

for _ in $(seq 1 60); do
  [[ -b "$state_device" ]] && break
  sleep 1
done
[[ -b "$state_device" ]] || {
  echo "stateful server disk did not appear: $state_device" >&2
  exit 1
}

fresh_disk="false"
if ! blkid "$state_device" >/dev/null 2>&1; then
  fresh_disk="true"
  mkfs.ext4 -F "$state_device"
fi

mkdir -p "$state_root"
mountpoint -q "$state_root" || mount -o noatime "$state_device" "$state_root"
mkdir -p "$state_root/consul" "$state_root/nomad"

if [[ "$allow_fresh_bootstrap" != "true" ]] &&
  { [[ "$fresh_disk" == "true" ]] || [[ ! -f "$consul_bootstrap_marker" ]] || [[ ! -f "$nomad_bootstrap_marker" ]]; }
then
  echo "fresh control-plane bootstrap is not authorized" >&2
  exit 1
fi

for service in consul nomad; do
  target="/opt/$service/data"
  source="$state_root/$service"
  chown --reference="$target" "$source"
  chmod --reference="$target" "$source"
  mountpoint -q "$target" || mount --bind "$source" "$target"
done

export CONSUL_ACL_BOOTSTRAP_MARKER="$consul_bootstrap_marker"
export NOMAD_ACL_BOOTSTRAP_MARKER="$nomad_bootstrap_marker"

gsutil cp "gs://${SCRIPTS_BUCKET}/run-consul-${RUN_CONSUL_FILE_HASH}.sh" /opt/consul/bin/run-consul.sh
gsutil cp "gs://${SCRIPTS_BUCKET}/run-nomad-${RUN_NOMAD_FILE_HASH}.sh" /opt/nomad/bin/run-nomad.sh

chmod +x /opt/consul/bin/run-consul.sh /opt/nomad/bin/run-nomad.sh

/opt/consul/bin/run-consul.sh --server --cluster-tag-name "${CLUSTER_TAG_NAME}" --consul-token "${CONSUL_TOKEN}" --enable-gossip-encryption --gossip-encryption-key "${CONSUL_GOSSIP_ENCRYPTION_KEY}"
/opt/nomad/bin/run-nomad.sh --server --num-servers "${NUM_SERVERS}" --consul-token "${CONSUL_TOKEN}" --nomad-token "${NOMAD_TOKEN}"
