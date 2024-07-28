#!/usr/bin/env bash
# 初始化 
cd /home/ubuntu/gopath/src/github.com/harmony-one/dlchain

tc qdisc add dev eth0 root tbf rate \
 30mbit burst 4mbit latency  1000000ms  
# tc qdisc add dev eth0 root tbf rate 1mbit burst 32kbit latency 400ms


set -eo pipefail

unset -v progdir
case "${0}" in
*/*) progdir="${0%/*}" ;;
*) progdir=. ;;
esac

ROOT="${progdir}/.."
USER=$(whoami)
OS=$(uname -s)
 


function setup() {
  # Setup blspass file
  mkdir -p ${ROOT}/.hmy
  if [[ ! -f "${ROOT}/.hmy/blspass.txt" ]]; then
    touch "${ROOT}/.hmy/blspass.txt"
  fi



  # Create a tmp folder for logs
  t=$(date +"%Y%m%d-%H%M")
  log_folder="${ROOT}/tmp_log/log"
  mkdir -p "${log_folder}"
  LOG_FILE=${log_folder}/r.log
}

BN_MA="/ip4/127.0.0.1/tcp/19876/p2p/Qmc1V6W7BwX8Ugb42Ti8RnXF1rY5PF7nnZ6bKBryCgi6cv"
function launch_localnet() {

  unset -v base_args
  declare -a base_args args

  verbosity=3

  base_args=(--log_folder "${log_folder}" --waiting_time "${waiting_time}" --waiting_time_meta "${waiting_time_meta}" --stop_main_flag "${stop_main_flag}" --min_peers "${MIN}" --bootnodes "${BN_MA}" "--network_type=$NETWORK" --blspass file:"${ROOT}/.hmy/blspass.txt" "--dns=false" "--verbosity=${verbosity}")
  sleep 2

  # Start nodes
  i=-1
  while IFS='' read -r line || [[ -n "$line" ]]; do
    i=$((i + 1))
    if [[ $i -ne $node_line ]]; then
      continue
    fi

    echo "handle node $i"

    if [[ $i -gt 2 ]]; then
      verbosity=0
      base_args=(--log_folder "${log_folder}" --waiting_time "${waiting_time}" --waiting_time_meta "${waiting_time_meta}" --stop_main_flag "${stop_main_flag}" --min_peers "${MIN}" --bootnodes "${BN_MA}" "--network_type=$NETWORK" --blspass file:"${ROOT}/.hmy/blspass.txt" "--dns=false" "--verbosity=${verbosity}")
    fi
    # Read config for i-th node form config file
    IFS=' ' read -r ip port mode bls_key shard <<<"${line}"
    
    args=("${base_args[@]}" --ip "${ip}" --port "${port}" --key "/tmp/${ip}-${port}.key" --db_dir "${ROOT}/db-${ip}-${port}" "--broadcast_invalid_tx=false")
    if [[ -z "$ip" || -z "$port" ]]; then
      echo "skip empty node"
      continue
    fi
    if [[ $EXPOSEAPIS == "true" ]]; then
      args=("${args[@]}" "--http.ip=0.0.0.0" "--ws.ip=0.0.0.0")
    fi

    # Setup BLS key for i-th localnet node
    if [[ ! -e "$bls_key" ]]; then
      args=("${args[@]}" --blskey_file "BLSKEY")
    elif [[ -f "$bls_key" ]]; then
      args=("${args[@]}" --blskey_file "${ROOT}/${bls_key}")
    elif [[ -d "$bls_key" ]]; then
      args=("${args[@]}" --blsfolder "${ROOT}/${bls_key}")
    else
      echo "skipping unknown node"
      continue
    fi

    # Setup flags for i-th node based on config
    case "${mode}" in
    explorer)
      args=("${args[@]}" "--node_type=explorer" "--http.rosetta=true" "--run.archive")
      ;;
    archival)
      args=("${args[@]}" --is_archival --run.legacy)
      ;;
    leader)
      args=("${args[@]}" --is_leader --run.legacy)
      ;;
    external)
      ;;
    client)
      args=("${args[@]}" --run.legacy)
      ;;
    validator)
      args=("${args[@]}" --run.legacy)
      ;;
    esac

    "/home/ubuntu/gopath/src/github.com/harmony-one/dlchain/bin/harmony" "${args[@]}" "${extra_args[@]}" 2>&1 | tee -a "${LOG_FILE}" &
    break
  done <"${config}"
}

trap cleanup SIGINT SIGTERM

function usage() {
  local ME=$(basename $0)

  echo "
USAGE: $ME [OPTIONS] config_file_name [extra args to node]

   -h             print this help message
   -D duration    test run duration (default: $DURATION)
   -m min_peers   minimal number of peers to start consensus (default: $MIN)
   -s shards      number of shards (default: $SHARDS)
   -n             dryrun mode (default: $DRYRUN)
   -N network     network type (default: $NETWORK)
   -B             don't build the binary
   -v             verbosity in log (default: $VERBOSE)
   -e             expose WS & HTTP ip (default: $EXPOSEAPIS)

This script will build all the binaries and start harmony and based on the configuration file.

EXAMPLES:

   $ME local_config.txt
"
  exit 0
}

DURATION=60000
MIN=3
SHARDS=2
DRYRUN=
NETWORK=localnet
VERBOSE=false
NOBUILD=false
EXPOSEAPIS=false

while getopts "hD:m:s:nBN:ve" option; do
  case ${option} in
  h) usage ;;
  D) DURATION=$OPTARG ;;
  m) MIN=$OPTARG ;;
  s) SHARDS=$OPTARG ;;
  n) DRYRUN=echo ;;
  B) NOBUILD=true ;;
  N) NETWORK=$OPTARG ;;
  v) VERBOSE=false ;;
  e) EXPOSEAPIS=true ;;
  *) usage ;;
  esac
done

shift $((OPTIND - 1))

config=$1
node_line=$2
waiting_time=$3
waiting_time_meta=$4
stop_main_flag=$5

shift 5 || usage
unset -v extra_args
declare -a extra_args
extra_args=("$@")

setup
launch_localnet
sleep "${DURATION}"
cleanup || true
