#!/usr/bin/env bash

./test/kill_node.sh
sudo rm -rf tmp_log*
sudo rm *.rlp
sudo rm -rf .dht*
sudo rm -rf db-*
scripts/go_executable_build.sh -S harmony || exit 1  # dynamic builds are faster for debug iteration...
./test/deploy.sh -B -D 600000 ./test/configs/local-resharding.txt  2 3 5