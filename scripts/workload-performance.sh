#!/bin/sh

# Workload performance testing (memory, disk, cpu, network)

sudo apt update
sudo apt install -y make gcc git

wget https://go.dev/dl/go1.26.3.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.26.3.linux-amd64.tar.gz

echo "export PATH=$PATH:/usr/local/go/bin" >> $HOME/.bashrc
source $HOME/.bashrc

git clone https://github.com/dgraph-io/badger.git
cd badger

# disable jemalloc
sed -i 's/-tags=jemalloc/-tags=nojemalloc/g' test.sh

# run 4 tests, otherwise OOM on <8GB RAM
sed -i 's/-parallel 16/-parallel 4/g; s/-parallel=16/-parallel=4/g' test.sh

# run tests
./test.sh
