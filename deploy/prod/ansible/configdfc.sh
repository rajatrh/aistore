#!/bin/bash
set -e

export LOGDIR=/var/log/ais
export CONFDIR=/ais
export CONFFILE=$HOME/ais.json
export CLDPROVIDER=aws
export PORT=8081
export PROXY=`cat ./inventory/proxy.txt`
export PROXYURL='http://'$PROXY':8081'
export CONFFILE_STATSD=$HOME/statsd.json
export CONFFILE_COLLECTD=$HOME/collectd.json
export GRAPHITE_SERVER=`cat ./inventory/graphana.txt`
FSP=
for disk in "$@"; do
    if [ -z "$FSP" ]; then
	FSP='"/ais/'$disk'": " "'
    else
        FSP=$FSP', "/ais/'$disk'": " "'
    fi
done
echo FSPATHS are $FSP
#export FSPATHS='"/ais/xvdb": " ", "/ais/xvdc": " ", "/ais/xvdd": " ", "/ais/xvde": " "'
export FSPATHS=$FSP
export IPV4LIST=$(awk -vORS=, '{ print $1 }' ./inventory/cluster.txt | sed 's/,$//')
sudo rm -rf aisproxy.json || true
sudo rm -rf ais.json || true
source /etc/profile.d/aispaths.sh
$AISSRC/setup/config.sh

