#!/usr/bin/env bash

docker stop $(docker ps -aq)
bash ./test/rebuild.sh
bash ./test/buildboot.sh
# 获得172 ip地址
externalIP=`ifconfig ens5 | grep "inet " | awk '{ print $2}'  `


node_file="/home/ubuntu/gopath/src/github.com/harmony-one/dlchain/test/configs/local-resharding.txt"

# waiting time
echo $1 
waiting_time=$1
waiting_time_meta=$2
stop_main_flag=$3

file_lines=`cat $node_file | wc -l`
echo $file_lines

i=0
while IFS='' read -r line || [[ -n "$line" ]]; do
    
    IFS=' ' read -r ip port mode bls_key shard <<<"${line}"
    echo "line $i port is $port"
    docker run -p 0.0.0.0:"$port":"$port" --env sysExternalIP=$externalIP --cap-add=NET_ADMIN -i -v  /home/ubuntu/gopath:/home/ubuntu/gopath harmony_baseline:v2  /bin/bash /home/ubuntu/gopath/src/github.com/harmony-one/dlchain/test/setcommonDocker.sh -B -D 600000  $node_file $i $waiting_time $waiting_time_meta $stop_main_flag  &
    let i++
done <"${node_file}"

sleep 600000
