FROM harmony_baseline:v1

SHELL ["/bin/bash", "-c"]

# RUN apt update -y 
# RUN apt install net-tools 
# RUN apt install iproute2 -y 
# RUN apt install -y libssl-dev


ENV MCL_DIR=/home/ubuntu/gopath/src/github.com/harmony-one/mcl
ENV BLS_DIR=/home/ubuntu/gopath/src/github.com/harmony-one/bls
ENV CGO_CFLAGS="-I/home/ubuntu/gopath/src/github.com/harmony-one/bls/include -I/home/ubuntu/gopath/src/github.com/harmony-one/mcl/include"
ENV CGO_LDFLAGS="-L/home/ubuntu/gopath/src/github.com/harmony-one/bls/lib"
ENV LD_LIBRARY_PATH=/home/ubuntu/gopath/src/github.com/harmony-one/bls/lib:/home/ubuntu/gopath/src/github.com/harmony-one/mcl/lib


# RUN tc qdisc add dev eth0 root tbf rate 20480kbit latency 50ms burst 163840