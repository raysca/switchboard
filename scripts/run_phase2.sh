#!/bin/bash
# Phase 2: runs after T8. Usage: run_phase2.sh <best-mode> <workers> <batch> <rt-ceiling>
cd "$(dirname "$0")/.." && export GOVOICE_ROOT=$PWD
M=$1; W=$2; B=$3; C=$4
CFG="-mode $M -workers $W -batch $B"
N80=$((C*8/10))
echo "=== stub control"
./bin/bench vad -mode stub -workers 11 -batch 1 -stubcost 75us -window 10s -long 30s -streams 500,1000,2000,3000,4000,5000,6000,7000,8000,9000,10000 2>&1
echo "=== T10 combined (fakes)"
./bin/bench vad $CFG -fakes -window 10s -long 30s -streams $((C/2)),$((C*7/10)),$((C*8/10)),$((C*9/10)),$C -name t10_combined.csv 2>&1
for d in 0.1 0.25 0.5; do
  echo "=== T9 duty $d gate on"
  ./bin/bench vad $CFG -duty $d -gate 0.002 -window 10s -long 30s -streams 2000,4000,6000,8000,10000,12000,14000,16000,20000,24000 -name t9_duty$d.csv 2>&1
done
echo "=== T9 gate off (duty irrelevant: every chunk inferred)"
cp results/t7_${M}-w${W}-b${B}-i1.csv results/t9_duty1.0_gateoff.csv
echo "=== T5/T6 containment at 80% ceiling ($N80)"
./bin/bench contain $CFG -n $N80 -window 15s 2>&1
echo "=== T4 churn"
./bin/bench churn $CFG -n 200 -cycles 20000 2>&1 | awk 'NR%10==1'
echo "=== T11 admission off"
./bin/bench admission $CFG -ceiling $C -admission=false 2>&1 | tail -3
echo "=== T11 admission on"
./bin/bench admission $CFG -ceiling $C -admission=true 2>&1 | tail -3
echo "=== isolation"
./bin/bench isolation -workers 8 -batch 8 -n 2800 2>&1
echo DONE
