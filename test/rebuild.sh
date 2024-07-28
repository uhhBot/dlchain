#!/usr/bin/env bash
#used to kill nodes and build the binary before boot up

sudo ./test/kill_node.sh
sudo rm -rf tmp_log*
sudo rm *.rlp
sudo rm -rf .dht*
sudo rm -rf db-*
scripts/go_executable_build.sh -S  harmony || exit 1  # dynamic builds are faster for debug iteration...