#!/usr/bin/env bash

HELP_TEXT=\'''${0##*/}''\'
HELP_TEXT+=$' scans a running Contiv cluster and generates test data used
in validator unit tests. The test data is generated in json format and
unmarshalled by test setup routines in \'validator_test.g\'. The script is
typically run when type definitions for data in CRD caches changes. The
generated files should be checked in.

The script generates the following files:
- pod_raw_data_test.go:     contains K8s pod data obtained from etcd, where
                            it has been dumped by Ksr
- k8snode_raw_data_test.go: contains K8s node data obtained from etcd, where
                            it has been dumped by Ksr
- node_raw_data_test.go:    contains VPP dump data gleaned from Contiv VPP
                            Agents on every node in the cluster

Prerequisites are:
- Running & properly functioning Contiv cluster
- etcdctl (install with \'go get github.com/coreos/etcd/etcdctl\')
- python (>= 2.7)
- kubectl (install with \'kubeadm\')
'
set -euo pipefail

usage() {
    echo "Usage: '${0##*/} [OPTION]...'"
    echo
    echo "Available options:"
    echo
    echo "-h  Show this help message."
    echo
    echo "$HELP_TEXT"
}

get_data() {
    data=$( curl -s "$1$VPP_DUMP_PFX$2"| python -mjson.tool | sed -e 's|    |\t|g' | sed -e 's/\(^[\t}].*$\)/\t\t\t\1/')
    echo "$data"
}

while getopts "h" opt
do
    case "$opt" in
    h)
        usage
        exit 0
        ;;
    *)
        # getopts will have already displayed a "illegal option" error.
        echo
        usage
        exit 1
        ;;
    esac
done

# check that etcd is installed
command -v etcdctl >/dev/null 2>&1 || {
    echo >&2 "'etcdctl' required but not found. Install with 'go get github.com/coreos/etcd/etcdctl'."
    exit 1
}

############################
# Generate raw VPP dump data
############################
echo Generating VPP dump data...

NODES=()
declare -A NODE_IP_ADDRESSES

# Temporary, until IPAM works from the node
ALLOCATED_IDS_PFX="/vnf-agent/contiv-ksr/allocatedIDs/"
NODE_INFO=$( etcdctl --endpoints=127.0.0.1:32379 get "$ALLOCATED_IDS_PFX" --prefix=true |grep -v "$ALLOCATED_IDS_PFX" )

VSWITCHES=$( kubectl get pods -o wide --all-namespaces | grep "contiv-vswitch" )
readarray -t VSWITCH_LINES <<< "$VSWITCHES"
for l in "${VSWITCH_LINES[@]}"
do
    IFS=' ' read -ra NODE_FIELDS <<< "$l"
    NODE="${NODE_FIELDS[7]}"
    NODES+=("$NODE")
    NODE_IP_ADDRESSES["$NODE"]="${NODE_FIELDS[6]}"
done

# for K in "${!NODE_IP_ADDRESSES[@]}"; do echo $K --- ${NODE_IP_ADDRESSES[$K]}; done
VT_NODE_RAW_DATA="// Code generated by '${0##*/}' on $( date ). DO NOT EDIT."
VT_NODE_RAW_DATA+=$'
//
// Image versions:'

VPP_DUMP_PFX=":9999/vpp/dump/v1/"
for nn in "${NODES[@]}"
do
    IP_ADDR=${NODE_IP_ADDRESSES[$nn]}
    LIVENESS=$( curl -s "$IP_ADDR":9999/liveness | python -mjson.tool )
    BUILD_VERSION=$( echo "$LIVENESS" | grep "build_version" | awk '{print $2}' | sed -e 's|[",]||g' )
    echo Node "$nn" build version: "$BUILD_VERSION"
    VT_NODE_RAW_DATA+=$( printf "\n// - %s:\t%s" "$nn" "$BUILD_VERSION" )
done

VT_NODE_RAW_DATA+=$'

package testdata

type rawNodeTestData map[string]map[string]string

func getRawNodeTestData() rawNodeTestData {
\treturn rawNodeTestData{
'

VPP_DUMP_PFX=":9999/vpp/dump/v1/"
for nn in "${NODES[@]}"
do
    IP_ADDR=${NODE_IP_ADDRESSES[$nn]}
    VT_NODE_RAW_DATA+=$'\t\t"'"$nn"$'": {\n'

    # Temporary, until we can get ID from node IPAM data
    NODEINFO=$( echo "$NODE_INFO" | grep "$nn" | python -mjson.tool | sed -e 's|    |\t|g' | sed -e 's/\(^[\t}].*$\)/\t\t\t\1/' )

    # Get data from the node
    LIVENESS=$( curl -s "$IP_ADDR":9999/liveness | python -mjson.tool | sed -e 's|    |\t|g' | sed -e 's/\(^[\t}].*$\)/\t\t\t\1/' )
    IPAM=$( curl -s "$IP_ADDR":9999/contiv/v1/ipam | python -mjson.tool | sed -e 's|    |\t|g' | sed -e 's/\(^[\t}].*$\)/\t\t\t\1/' )
    INTERFACES=$( get_data "$IP_ADDR" "interfaces" )
    BD=$( get_data "$IP_ADDR" "bd" )
    L2FIB=$( get_data "$IP_ADDR" "fib" )
    ARPS=$( get_data "$IP_ADDR" "arps" )
    ROUTES=$( get_data "$IP_ADDR" "routes" )

    # Create the data structure for the node
    VT_NODE_RAW_DATA+=$( printf "\t\t\t\"nodeinfo\": \`%s\`,\n" "$NODEINFO" )
    VT_NODE_RAW_DATA+=$( printf "\n\t\t\t\"liveness\": \`%s\`,\n" "$LIVENESS" )
    VT_NODE_RAW_DATA+=$( printf "\n\t\t\t\"ipam\": \`%s\`,\n" "$IPAM" )
    VT_NODE_RAW_DATA+=$( printf "\n\t\t\t\"interfaces\": \`%s\`,\n" "$INTERFACES" )
    VT_NODE_RAW_DATA+=$( printf "\n\t\t\t\"bridgedomains\": \`%s\`,\n" "$BD" )
    VT_NODE_RAW_DATA+=$( printf "\n\t\t\t\"l2fib\": \`%s\`,\n" "$L2FIB" )
    VT_NODE_RAW_DATA+=$( printf "\n\t\t\t\"arps\": \`%s\`,\n" "$ARPS" )
    VT_NODE_RAW_DATA+=$( printf "\n\t\t\t\"routes\": \`%s\`,\n" "$ROUTES" )

    VT_NODE_RAW_DATA+=$'\n\t\t},\n'
done

VT_NODE_RAW_DATA+=$'\t}\n}'

# echo "$VT_RAW_DATA"
echo "$VT_NODE_RAW_DATA" > contiv_node_raw_data.go


################################
# Generate raw k8s pod test data
################################

echo Generating k8s pod data...

VT_POD_RAW_DATA="// Code generated by '${0##*/}' on $( date ) DO NOT EDIT."
VT_POD_RAW_DATA+=$'

package testdata

func getRawK8sPodTestData() []string {
\treturn []string{
'

ETCD_K8S_POD_PFX="/vnf-agent/contiv-ksr/k8s/pod/"
POD_INFO=$( etcdctl --endpoints=127.0.0.1:32379 get "$ETCD_K8S_POD_PFX" --prefix=true |grep -v "$ETCD_K8S_POD_PFX" )

readarray -t POD_LINES <<< "$POD_INFO"
for l in "${POD_LINES[@]}"
do
    POD=$( echo "$l" | python -mjson.tool | sed -e 's|    |\t|g' | sed -e 's/\(^[\t}].*$\)/\t\t\1/' )
    VT_POD_RAW_DATA+=$( printf "\n\t\t\`%s\`,\n" "$POD" )
done

VT_POD_RAW_DATA+=$'\n\t}\n}'

# echo "$VT_POD_RAW_DATA"
echo "$VT_POD_RAW_DATA" > k8s_pod_raw_data.go


################################
# Generate raw k8s node test data
################################

echo Generating k8s node data...

VT_K8SNODE_RAW_DATA="// Code generated by '${0##*/}' on $( date ). DO NOT EDIT."
VT_K8SNODE_RAW_DATA+=$'

package testdata

func getRawK8sNodeTestData() []string {
\treturn []string{
'

ETCD_K8S_NODE_PFX="/vnf-agent/contiv-ksr/k8s/node/"
K8SNODE_INFO=$( etcdctl --endpoints=127.0.0.1:32379 get "$ETCD_K8S_NODE_PFX" --prefix=true |grep -v "$ETCD_K8S_NODE_PFX" )

readarray -t K8SNODE_LINES <<< "$K8SNODE_INFO"
for l in "${K8SNODE_LINES[@]}"
do
    K8SNODE=$( echo "$l" | python -mjson.tool | sed -e 's|    |\t|g' | sed -e 's/\(^[\t}].*$\)/\t\t\1/' )
    VT_K8SNODE_RAW_DATA+=$( printf "\n\t\t\`%s\`,\n" "$K8SNODE" )
done

VT_K8SNODE_RAW_DATA+=$'\n\t}\n}'

echo "$VT_K8SNODE_RAW_DATA" > k8s_node_raw_data.go

