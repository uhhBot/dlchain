## Description

This is the prototype implementation of DL-Chain, based on [Harmony](https://github.com/harmony-one/harmony).

  
## Requirements

### **Go 1.16.3**
### **GMP and OpenSSL**

 
On Linux (Ubuntu)
```bash
sudo apt install libgmp-dev  libssl-dev  make gcc g++
```

## Dev Environment
The dev envirnoment is the same to harmony-one, as follows.

**Most repos from [harmony-one](https://github.com/harmony-one) assumes the GOPATH convention. More information [here](https://github.com/golang/go/wiki/GOPATH).** 

### First Install
Clone and set up all of the repos with the following set of commands:

1. Create the appropriate directories:
```bash
mkdir -p $(go env GOPATH)/src/github.com/harmony-one
cd $(go env GOPATH)/src/github.com/harmony-one
```
> If you get 'unknown command' or something along those lines, make sure to install [golang](https://golang.org/doc/install) first. 

2. Clone this repo & dependent repos.
```bash
git clone https://github.com/harmony-one/mcl.git
git clone https://github.com/harmony-one/bls.git
git clone https://github.com/uhhBot/dlchain.git
cd dlchain
```

3. Build the harmony binary & dependent libs
```
go mod tidy
make
```
> Run `bash scripts/install_build_tools.sh` to ensure build tools are of correct versions.
> If you get 'missing go.sum entry for module providing package <package_name>', run `go mod tidy`.


 
## Preparation

Please search for ```/home/ubuntu/gopath/src/github.com/harmony-one/dlchain``` and replace it with the absolute path of the project on your device.

## Run

Please run ```make debug``` in the directory where the ```Makefile``` is located.

> This localnet has 2 shards (finalizer committees), with 20 nodes on each.

The network's running status will be recorded in ```/tmp_log/log```. This test network includes two finalizer committees (leader logs recorded in ```zerolog-validator-0.0.0.0-9000.log``` and ```zerolog-validator-0.0.0.0-9001.log```), and a total of four proposer shards (leader logs recorded in ```zerolog-validator-0.0.0.0-9004.log``` to ```zerolog-validator-0.0.0.0-9007.log```).