#!/usr/bin/env bash

# no need to rebuild before boot
###### build the binary
sudo ./test/kill_node.sh
sudo rm -rf tmp_log*
sudo rm *.rlp
sudo rm -rf .dht*
sudo rm -rf db-*
# no need to build
# scripts/go_executable_build.sh -S || exit 1  # dynamic builds are faster for debug iteration...
######

set -eo pipefail

unset -v progdir
case "${0}" in
*/*) progdir="${0%/*}" ;;
*) progdir=. ;;
esac

ROOT="${progdir}/.."
USER=$(whoami)
OS=$(uname -s)
. "${ROOT}/scripts/setup_bls_build_flags.sh"

function setup() {
  # Setup blspass file
  mkdir -p ${ROOT}/.hmy
  if [[ ! -f "${ROOT}/.hmy/blspass.txt" ]]; then
    touch "${ROOT}/.hmy/blspass.txt"
  fi
  
  # Create a tmp folder for logs
  t=$(date +"%Y%m%d-%H%M%S")
  log_folder="${ROOT}/tmp_log/log-$t"
  mkdir -p "${log_folder}"
  LOG_FILE=${log_folder}/r.log
}
function launch_bootnode() {
  echo "launching boot node ..."
  ${DRYRUN} ${ROOT}/bin/bootnode -ip 0.0.0.0 -port 19876 >"${log_folder}"/bootnode.log 2>&1 | tee -a "${LOG_FILE}" &
  sleep 1
  BN_MA=$(grep "BN_MA" "${log_folder}"/bootnode.log | awk -F\= ' { print $2 } ')
  echo "bootnode launched." + " $BN_MA"
}
setup
launch_bootnode