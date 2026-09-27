#!/bin/bash
# T7/T8: VAD scheduler matrix. Each config in a fresh process.
cd "$(dirname "$0")/.." && export GOVOICE_ROOT=$PWD
G="500,1000,2000,3000,3500,4000,4500,5000,5500,6000,6500,7000,8000,9000"
run() { echo "=== $*"; ./bin/bench vad -window 10s -long 30s -streams $G "$@" 2>&1 | grep -v "^$"; }
run -mode stub -workers 11 -batch 1 -stubcost 75us
run -mode global -workers 11 -batch 8
run -mode global -workers 11 -batch 1
run -mode global -workers 8 -batch 8
run -mode global -workers 8 -batch 1
run -mode sharded -workers 11 -batch 8
run -mode sharded -workers 11 -batch 1
run -mode process -workers 8 -batch 8
run -mode process -workers 4 -batch 8
run -mode global -workers 4 -batch 8 -intra 3
run -mode stub -workers 11 -batch 1 -stubcost 75us
echo DONE
