#!/usr/bin/env bash

# clean docker
docker stop $(docker ps -aq)
# sudo ./test/kill_node.sh
sudo rm -rf tmp_log*
sudo rm *.rlp
sudo rm -rf .dht*
sudo rm -rf db-*

# 获得172 ip地址
externalIP=`ifconfig | grep 255.255.240.0 | grep "inet " | awk '{ print $2}'  `
# externalIP=`ifconfig ens1 | grep "inet " | awk '{ print $2}'  `

echo $1 
node_file=$1

# waiting time
echo $2 
waiting_time=$2

waiting_time_meta=$3
stop_main_flag=$4

file_lines=`cat $node_file | wc -l`
echo $file_lines

i=0
while IFS='' read -r line || [[ -n "$line" ]]; do
    
    IFS=' ' read -r ip port mode bls_key shard <<<"${line}"
    echo "line $i port is $port"
    docker run -p 0.0.0.0:"$port":"$port" --env sysExternalIP=$externalIP --cap-add=NET_ADMIN -i -v  /home/ubuntu/gopath:/home/ubuntu/gopath harmony_baseline:v2  /bin/bash /home/ubuntu/gopath/src/github.com/harmony-one/dlchain/test/setcommonDocker.sh -B -D 600000  $node_file $i $waiting_time $waiting_time_meta  $stop_main_flag &
    # docker run  --cap-add=NET_ADMIN -i -v  /home/ubuntu/gopath:/home/ubuntu/gopath harmony_baseline:v2  /bin/bash /home/ubuntu/gopath/src/github.com/harmony-one/harmony_baseline/test/setcommonDocker.sh -B -D 600000  $node_file $i &
    # --net=host
    
    let i++
done <"${node_file}"

sleep 600000
