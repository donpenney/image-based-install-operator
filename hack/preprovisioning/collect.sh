#!/bin/bash
# Copyright 2026 Red Hat, Inc.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
umask 077

if [ "$#" -ne 4 ]; then
    echo "Usage: bash collect.sh KUBECONFIG BMH_NAMESPACE BMH_NAME" \
        "NEW_OUTPUT_DIR" >&2
    exit 1
fi

probe_kubeconfig=$1
probe_namespace=$2
probe_host=$3
probe_output=$4

test -r "${probe_kubeconfig}"
command -v oc >/dev/null
command -v jq >/dev/null
# Refuse to overwrite an existing collection or follow an output symlink.
mkdir -- "${probe_output}"

oc_probe() {
    oc --kubeconfig="${probe_kubeconfig}" --request-timeout=30s get "$@"
}

date -u +%FT%TZ > "${probe_output}/collected-at.txt"
oc_probe clusterversion version -o json |
    jq '{version: .status.desired.version}' \
        > "${probe_output}/openshift.json"
oc_probe deployments -A -o json |
    jq '[.items[] |
        select(.metadata.name | test("metal3|image-based-install")) |
        {namespace: .metadata.namespace, name: .metadata.name,
            containers: [.spec.template.spec.containers[] | {name, image}]}]' \
        > "${probe_output}/components.json"
oc_probe baremetalhosts "${probe_host}" -n "${probe_namespace}" -o json |
    jq '{apiVersion, kind,
        metadata: (.metadata | {name, namespace, uid, generation,
            deletionTimestamp}),
        spec: (.spec | {online, externallyProvisioned, image, bootMode}),
        status: (.status | {poweredOn, provisioning, operationalStatus,
            errorType, errorCount, errorMessage})}' \
        > "${probe_output}/baremetalhost.json"
oc_probe dataimages "${probe_host}" -n "${probe_namespace}" \
    --ignore-not-found -o json > "${probe_output}/dataimage.json"
if [ ! -s "${probe_output}/dataimage.json" ]; then
    echo '{"present":false}' > "${probe_output}/dataimage.json"
fi
oc_probe events -n "${probe_namespace}" \
    --field-selector="involvedObject.name=${probe_host}" -o json |
    jq '[.items[] | {eventTime, firstTimestamp, lastTimestamp,
        involvedObject: (.involvedObject | {kind, name, uid}),
        type, reason, message}]' > "${probe_output}/events.json"

echo "Read-only evidence saved to ${probe_output}"
