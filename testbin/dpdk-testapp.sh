#!/usr/bin/env bash

set -o errexit
set -o pipefail

# When enabled, pin server and client to non-sibling CPUs.
# When disabled (default), use all CPUs listed in the PowerNode CR (e.g., HT disabled scenarios).
NON_SIBLINGS=false

function setup_dpdk_testapp {
    oc apply -f ../examples/example-dpdk-testapp.yaml
    oc wait -n intel-power --for=condition=available --timeout=180s deployment/dpdk-testapp

    POD=$(oc get pods -n intel-power -l app=dpdk-testapp \
        -ojsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -n 1)

    # Wait for the pod to be running
    oc wait --for=jsonpath='{.status.phase}'=Running pod/"$POD" -n intel-power --timeout=120s

    NODE=$(oc get pods -n intel-power -l app=dpdk-testapp \
        -ojsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | head -n 1)

    # Wait until PowerNode has exclusive CPUs for server
    SERVER_CPUS=""
    while true; do
        SERVER_CPUS_RAW=$(oc get powernode "$NODE" -n intel-power -o \
            jsonpath='{range .status.powerContainers[?(@.name=="server")]}{.exclusiveCpus}{end}' 2>/dev/null)
        if [ -n "$SERVER_CPUS_RAW" ]; then
            SERVER_LIST=$(echo "$SERVER_CPUS_RAW" | tr -d '[]' | tr ',' ' ')
            if $NON_SIBLINGS; then
                SERVER_COUNT=$(echo "$SERVER_LIST" | wc -w | awk '{print $1}')
                if [ "$SERVER_COUNT" -gt 1 ]; then
                    HALF=$((SERVER_COUNT/2))
                else
                    HALF=$SERVER_COUNT
                fi
                SERVER_SELECTED=$(echo "$SERVER_LIST" | tr ' ' '\n' | head -n "$HALF" | paste -sd, -)
            else
                SERVER_SELECTED=$(echo "$SERVER_LIST" | tr ' ' '\n' | paste -sd, -)
            fi
            SERVER_CPUS=""
            IFS=','
            for cpu in $SERVER_SELECTED; do
                if [ -n "$SERVER_CPUS" ]; then SERVER_CPUS+=","; fi
                SERVER_CPUS+="${cpu}@${cpu}"
            done
            unset IFS
        fi

        if [ -n "$SERVER_CPUS" ]; then break; fi
        echo "PowerNode not yet updated (server). Retrying..."
        sleep 2
    done
    # Number of server lcores (minus one control thread)
    SERVER_CPUS_NUM=$(($(echo "$SERVER_CPUS" | grep -o '@' | wc -l) - 1))

    # Wait until PowerNode has exclusive CPUs for client
    CLIENT_CPUS=""
    while true; do
        CLIENT_CPUS_RAW=$(oc get powernode "$NODE" -n intel-power -o \
            jsonpath='{range .status.powerContainers[?(@.name=="client")]}{.exclusiveCpus}{end}' 2>/dev/null)
        if [ -n "$CLIENT_CPUS_RAW" ]; then
            CLIENT_LIST=$(echo "$CLIENT_CPUS_RAW" | tr -d '[]' | tr ',' ' ')
            if $NON_SIBLINGS; then
                CLIENT_COUNT=$(echo "$CLIENT_LIST" | wc -w | awk '{print $1}')
                if [ "$CLIENT_COUNT" -gt 1 ]; then
                    HALF=$((CLIENT_COUNT/2))
                else
                    HALF=$CLIENT_COUNT
                fi
                CLIENT_SELECTED=$(echo "$CLIENT_LIST" | tr ' ' '\n' | head -n "$HALF" | paste -sd, -)
            else
                CLIENT_SELECTED=$(echo "$CLIENT_LIST" | tr ' ' '\n' | paste -sd, -)
            fi
            CLIENT_CPUS=""
            IFS=','
            for cpu in $CLIENT_SELECTED; do
                if [ -n "$CLIENT_CPUS" ]; then CLIENT_CPUS+=","; fi
                CLIENT_CPUS+="${cpu}@${cpu}"
            done
            unset IFS
        fi

        if [ -n "$CLIENT_CPUS" ]; then break; fi
        echo "PowerNode not yet updated (client). Retrying..."
        sleep 2
    done
    # Number of client lcores (minus one control thread)
    CLIENT_CPUS_NUM=$(($(echo "$CLIENT_CPUS" | grep -o '@' | wc -l) - 1))

    # Server receives traffic, updates checksums and forwards packets back to client
    echo "Deploying server..."
    oc exec -n intel-power "$POD" -c server -- \
        tmux new-session -s server -d "dpdk-testpmd --no-pci --lcores $SERVER_CPUS --file-prefix=rte \
        --huge-dir=\"/hugepages-1Gi\" \
        --vdev=\"net_memif0,role=server,socket=/var/run/memif/memif1.sock\" -- \
        --rxq=$SERVER_CPUS_NUM --txq=$SERVER_CPUS_NUM --nb-cores=$SERVER_CPUS_NUM \
        --interactive --rss-udp --forward-mode=csum --record-core-cycles --record-burst-stats"
    echo "Server CPUS: $SERVER_CPUS"
    sleep 1

    # Client generates traffic and transmits to server
    echo "Deploying client..."
    oc exec -n intel-power "$POD" -c client -- \
        tmux new-session -s client -d "dpdk-testpmd --no-pci --lcores $CLIENT_CPUS --file-prefix=client \
        --huge-dir=\"/hugepages-1Gi\" \
        --vdev=\"net_memif0,role=client,socket=/var/run/memif/memif1.sock\" -- \
        --rxq=$SERVER_CPUS_NUM --txq=$SERVER_CPUS_NUM --nb-cores=$CLIENT_CPUS_NUM \
        --interactive --rss-udp --forward-mode=txonly"
    echo "Client CPUS: $CLIENT_CPUS"

    # Push heavier traffic from the client
    oc exec -n intel-power "$POD" -c client -- tmux send-keys -t client "set burst 256" C-m
    # Start packet generation from client
    oc exec -n intel-power "$POD" -c client -- tmux send-keys -t client "start" C-m
    # Start packet processing from server
    oc exec -n intel-power "$POD" -c server -- tmux send-keys -t server "start" C-m
    echo "Deployed successfully"
}

function delete_dpdk_testapp {
    oc delete -f ../examples/example-dpdk-testapp.yaml --ignore-not-found=true

    # Wait for the deployment to be deleted if it exists
    oc wait -n intel-power --for=delete deployment/dpdk-testapp --timeout=120s || true

    echo "Waiting for dpdk-testapp pods to terminate..."
    for i in {1..60}; do
        if [ -z "$(oc get pods -n intel-power -l app=dpdk-testapp --no-headers 2>/dev/null | awk 'NF>0{print}' | head -n1)" ]; then
            echo "All dpdk-testapp pods terminated."
            break
        fi
        sleep 2
    done
}

: "${KUBECONFIG:=$HOME/.kube/config}"

# Usage: dpdk-testapp.sh [-d] [--non-siblings]
while [[ $# -gt 0 ]]; do
    case "$1" in
        -d)
            delete_dpdk_testapp
            exit 0
            ;;
        --non-siblings)
            NON_SIBLINGS=true
            shift
            ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

setup_dpdk_testapp
exit 0
