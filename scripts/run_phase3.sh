#!/bin/bash
cd "$(dirname "$0")/.." && export GOVOICE_ROOT=$PWD
echo "=== T5/T6 containment rerun (fixed harness)"
./bin/bench contain -mode sharded -workers 11 -batch 8 -n 4000 -window 15s 2>&1
echo "=== T9 duty 0.5 confirm"
./bin/bench vad -mode sharded -workers 11 -batch 8 -duty 0.5 -gate 0.002 -window 30s -long 30s -streams 8000,10000 -name t9_duty0.5_confirm.csv 2>&1
echo DONE
