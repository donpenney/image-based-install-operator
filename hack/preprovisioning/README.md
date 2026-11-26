<!--
Copyright 2026 Red Hat, Inc.
SPDX-License-Identifier: Apache-2.0
-->

# IBI pre-provisioning media experiment

Development prototype for [CNF-30307](https://redhat.atlassian.net/browse/CNF-30307)
and the first checkpoint of the
[implementation plan](https://github.com/openshift-kni/oran-o2ims/pull/5357).
The immediate question is whether the deployed BMO/Ironic/BMC combination can
deliver the shared live ISO and existing configuration DataImage together,
early enough for networking to be applied before preparation starts.

Use a designated disposable server that is available for disk overwrite during
the later preparation tests. Confirm its physical identity and that no existing
BMH, ProvisioningRequest, or other installation owns it. The commands below
include explicit BMH mutations and a boot; the Go tool and collector themselves
perform no cluster or BMC mutations.

This checkpoint uses a diagnostic copy of the live ISO with the known disk
preparation service masked. Its configuration ISO contains a unique marker and
an empty networking payload in the existing `cluster-config` layout. It is not
a complete configuration ISO for normal IBI installation. Static networking,
authenticated callbacks, publication/retention qualification, and the real
installation handoff remain subsequent experiments within this work item.

## Prepare local artifacts

Run from the IBI-O repository root. Requirements are Go, `oc`, `jq`,
`coreos-installer`, and an existing IBI live ISO containing an SSH public key
whose matching private key is available for debugging. SSH in this first
experiment can use DHCP or the console; the later networking experiment must
prove operation without DHCP.

Keep generated artifacts, keys, and evidence in a private temporary directory:

```bash
umask 077
probe_dir=$(mktemp -d /tmp/ibi-media-probe.XXXXXX)
go build -o "$probe_dir/preprovisioning" ./hack/preprovisioning

probe_kubeconfig=/path/to/hub-kubeconfig
live_iso=/path/to/rhcos-ibi.iso
```

Extract the complete existing Ignition and mask its preparation service:

```bash
coreos-installer iso ignition show "$live_iso" > "$probe_dir/original.ign"
"$probe_dir/preprovisioning" mask-ignition \
  --input "$probe_dir/original.ign" --output "$probe_dir/probe.ign"
coreos-installer iso ignition embed --force \
  --ignition-file "$probe_dir/probe.ign" \
  --output "$probe_dir/ibi-media-probe.iso" "$live_iso"
coreos-installer iso ignition show "$probe_dir/ibi-media-probe.iso" \
  > "$probe_dir/embedded.ign"
jq -e '.systemd.units[] |
  select(.name == "install-rhcos-and-restore-seed.service") |
  .mask == true and (has("contents") | not) and (has("dropins") | not)' \
  "$probe_dir/embedded.ign"
```

The tool preserves the other embedded Ignition settings, including SSH access.
It rejects an absent or duplicate preparation unit and remote Ignition
merge/replace references. Inspect other enabled installation units and
`coreos-installer iso kargs show` output before booting an unfamiliar ISO;
masking this unit does not disable independent installation mechanisms.
See the [CoreOS Installer ISO commands](https://coreos.github.io/coreos-installer/cmd/iso/).

Build the diagnostic configuration ISO using the same ISO9660 backend, Rock
Ridge setting, and volume label as the existing configuration ISO generator:

```bash
probe_marker=$(cat /proc/sys/kernel/random/uuid)
printf '%s\n' "$probe_marker" > "$probe_dir/expected-marker.txt"
"$probe_dir/preprovisioning" config-iso \
  --marker "$probe_marker" --output "$probe_dir/configuration-probe.iso"
sha256sum "$probe_dir/ibi-media-probe.iso" \
  "$probe_dir/configuration-probe.iso" > "$probe_dir/local-sha256.txt"
```

Publish these two diagnostic files at distinct HTTPS URLs accessible to the
actual media-fetch path. Keep the original production ISO unchanged. Record
the source URLs and hashes. An HTTPS hash of an origin file alone does not
prove the later BMC-facing cached file or the lifetime of those bytes.

## Prepare the isolated BMH case

Copy the example manifests into the temporary directory and edit them there:

```bash
cp hack/preprovisioning/baremetalhost.yaml \
  hack/preprovisioning/dataimage.yaml "$probe_dir/"
```

Set the actual BMC address/driver, boot MAC, boot mode, credentials Secret name,
and the two published diagnostic URLs. The DataImage name and namespace must
match the BMH. Keep `online: false` and `externallyProvisioned: false`. A normal
ICI would let today's IBI-O set the host externally provisioned and start its
existing installation path; omit the ICI during this isolated media experiment.

Review existing BMC registrations across namespaces and confirm that aliases
or different Redfish System paths do not identify this same physical target:

```bash
oc --kubeconfig="$probe_kubeconfig" get baremetalhosts -A \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,BMC:.spec.bmc.address
```

After confirming the target, create the test namespace and its BMC credentials
using private username/password files supplied by your lab workflow:

```bash
oc --kubeconfig="$probe_kubeconfig" create namespace ibi-preparation-probe
oc --kubeconfig="$probe_kubeconfig" create secret generic bmc-credentials \
  -n ibi-preparation-probe \
  --from-file=username=/path/to/private/bmc-username \
  --from-file=password=/path/to/private/bmc-password
oc --kubeconfig="$probe_kubeconfig" create -f "$probe_dir/baremetalhost.yaml"
oc --kubeconfig="$probe_kubeconfig" create -f "$probe_dir/dataimage.yaml"
bash hack/preprovisioning/collect.sh "$probe_kubeconfig" \
  ibi-preparation-probe ibi-media-probe "$probe_dir/before-boot"
```

Using `create` makes an existing object a visible conflict rather than silently
updating another experiment. Normal BMO registration/inspection behavior still
depends on platform configuration; observe the actual power and provisioning
state even while the requested state is offline.

## Boot and observe both media

In separate terminals, watch the BMH and DataImage transitions. Record actual
power state as well as desired `online` state:

```bash
oc --kubeconfig="$probe_kubeconfig" get baremetalhost ibi-media-probe \
  -n ibi-preparation-probe -w \
  -o custom-columns=STATE:.status.provisioning.state,ONLINE:.spec.online,POWERED:.status.poweredOn,ERROR:.status.errorType
oc --kubeconfig="$probe_kubeconfig" get dataimage ibi-media-probe \
  -n ibi-preparation-probe -w \
  -o custom-columns=ATTACHED:.status.attachedImage.url,ERROR:.status.error.message
```

Start the designated target through BMO:

```bash
oc --kubeconfig="$probe_kubeconfig" patch baremetalhost ibi-media-probe \
  -n ibi-preparation-probe --type=merge -p '{"spec":{"online":true}}'
```

Record when the configuration DataImage actually attaches relative to the first
live ISO boot. In a checked BMO implementation, attachment runs during power-on
of an already provisioned/externally provisioned host. The live-ISO provisioning
path therefore needs an explicit experiment; creating the DataImage first is
not proof of attachment before that boot.

Capture each individual VirtualMedia resource using the links advertised by
the target's Redfish service. Collection expansion is not supported uniformly;
an unexpanded collection with only `@odata.id` links is insufficient. Save the
complete resource responses as `virtualmedia-*.json`, including `@odata.id`,
`Inserted`, `Image`, and `MediaTypes`. Use your existing authenticated, TLS-trusted
Redfish client and keep its credentials out of recorded commands and outputs.
Record the BMC model/firmware and capture times, and repeat the capture after
the system settles.

Combine the responses and check the actual BMC-facing URLs. Ironic may cache
and rehost the ISO, so these can differ from the two original publication URLs:

```bash
jq -s '.' "$probe_dir"/virtualmedia-*.json > "$probe_dir/virtualmedia.json"
"$probe_dir/preprovisioning" check-media \
  --input "$probe_dir/virtualmedia.json" \
  --boot-url http://ironic.example.com/actual-boot.iso \
  --data-url http://ironic.example.com/actual-configuration.iso \
  > "$probe_dir/media-result.json"
bash hack/preprovisioning/collect.sh "$probe_kubeconfig" \
  ibi-preparation-probe ibi-media-probe "$probe_dir/after-boot"
```

The check requires both expected images inserted in different identified
devices. Its output explicitly leaves guest visibility and byte verification
unverified. A positive DataImage status or a BMC advertising two slots does not
establish either property. Some provisioner status implementations report any
inserted CD/DVD without identifying the DataImage URL.

## Verify inside the live environment

Use the console or SSH with the key retained from the original ISO. Confirm
that the preparation unit is masked, inactive, and has not executed this boot:

```bash
systemctl show install-rhcos-and-restore-seed.service \
  -p LoadState -p ActiveState -p ExecMainStartTimestampMonotonic
lsblk -o NAME,TYPE,LABEL,FSTYPE,MOUNTPOINTS
sudo mkdir -p /run/ibi-media-probe
sudo mount -o ro /dev/disk/by-label/cluster-config /run/ibi-media-probe
cat /run/ibi-media-probe/ibi-media-probe.txt
sudo umount /run/ibi-media-probe
```

Expected unit values are `LoadState=masked`, `ActiveState=inactive`, and
`ExecMainStartTimestampMonotonic=0`. The marker must match the local
`expected-marker.txt` and belong to this boot/attempt. Record the BMC identity,
BMH UID, boot ID (`cat /proc/sys/kernel/random/boot_id`), guest device layout,
media contents, and active virtual media together. Check the live boot source;
merely SSHing to an already installed OS does not prove live ISO boot.

## Record the checkpoint result

| Observation | Evidence required |
| --- | --- |
| Versions | OCP, component digests, BMC model/firmware, installer. |
| Identity | Physical target, BMH UID, current guest boot ID. |
| Attachment timing | BMH/DataImage transitions relative to first boot. |
| Concurrent media | Stable captures showing distinct devices and both images. |
| Guest visibility | Live boot and the expected `cluster-config` marker. |
| Fetch path | Source/BMC-facing URLs and caching/rehosting behavior. |
| Outcome | Reproducible success or failure and required design changes. |

If a power cycle is needed to make the DataImage attach, record it as a
different case. A workflow that only obtains networking data after a first
network-dependent preparation boot does not satisfy the proposed bootstrap
contract. Do not use direct BMC attachment to present a failed BMO test as a
successful supported path.

Before deleting the experiment's BMH/namespace or issuing another power change,
record its media, finalizers, and observed power state and agree the cleanup
sequence for the tested combination. No cleanup commands are run automatically.
The diagnostic DataImage must be replaced by a real installation configuration
before attempting any later disk-boot handoff.

Store evidence outside Git: it can include private URLs and installation data.
Passing this checkpoint permits the networking/callback and real handoff
experiments; it does not complete CNF-30307 or qualify a supported product path.

## Local checks

```bash
go test ./hack/preprovisioning -count=1
shellcheck hack/preprovisioning/collect.sh
bashate hack/preprovisioning/collect.sh
markdownlint hack/preprovisioning/README.md
```
