#!/usr/bin/env bash
node_file=$1

./test/kill_node.sh
rm -rf tmp_log*
rm *.rlp
rm -rf .dht*
scripts/go_executable_build.sh -S || exit 1  # dynamic builds are faster for debug iteration...
./test/setcommon.sh -B -D 600000 $node_file
# ./test/configs/local-resharding.txt