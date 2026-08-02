#!/bin/sh

# Performance scripts
sudo apt update
sudo apt install -y fio sysstat jq util-linux

# make sure not written to tmp disk, etc
mkdir $HOME/bench

# Require larger size (4G) since smaller tests will not correctly stress nbdkit/viperblock/predastore stack
fio --name=randrw_70_30 \
    --directory=$HOME/bench \
    --rw=randrw \
    --rwmixread=70 \
    --bs=4k \
    --size=4G \
    --numjobs=4 \
    --iodepth=32 \
    --ioengine=libaio \
    --direct=1 \
    --group_reporting
