# Automated IBI server preparation

This development workflow prepares a server with an IBI live ISO before the
normal ImageClusterInstall configuration workflow. It requires the matching
BMO, Ironic and Ironic runtime-image changes for concurrent virtual media.
Install the updated IBI-O CRD before deploying the new binaries. BMO uses
controller-owned annotations for its durable media ownership and does not
require a CRD change.

Preparation is optional. An ImageClusterInstall without `preProvisioning`
continues through the existing installation workflow.

## Inputs

Add the following block to an otherwise complete ImageClusterInstall before
creating it:

```yaml
spec:
  preProvisioning:
    isoURL: https://iso.example.com/ibi/rhcos-ibi-preparation.iso
    isoDigest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    isoServerCACertRef:
      name: iso-server-ca
    timeout: 60m
    maxRetries: 0
    retryBackoff: 5m
```

The optional CA reference identifies a ConfigMap in the ImageClusterInstall
namespace with a `ca-bundle.crt` entry. The source must be an unauthenticated
HTTPS URL on an operator-allowed origin. Redirects are rejected; use the final
URL. Preparation verifies the ISO digest before requesting boot and serves
those verified bytes through the existing internal image service.

The referenced BareMetalHost must be offline, have no image, and be available
for this installation. Another ImageClusterInstall, an existing IBI installation,
or a conflicting preparation claim prevents preparation. An already externally
provisioned host is returned to available state with automated cleaning disabled
before the live deployment is requested.

## Operator configuration and networking

The manager uses these operator-owned environment settings:

| Setting | Purpose |
| --- | --- |
| `PREPARATION_BASE_URL` | Callback HTTPS URL reachable from the live host. |
| `PREPARATION_CA_FILE` | Optional PEM bundle for callback TLS trust. |
| `PREPARATION_ISO_ORIGINS` | Allowed HTTPS ISO origins, including ports. |

These settings are independent of template and request authors. Freeze the
endpoint and its trust configuration for active attempts during configuration
or certificate changes.

The server has two separate listeners:

- The existing internal image listener on port 8000 serves normal installation
  images and preparation boot/configuration images. Ironic downloads these
  images and publishes its own BMC-facing copies.
- The `image-based-install-preparation` Service on port 8444 accepts authenticated
  preparation callbacks. It does not serve either kind of ISO or expose the
  normal `/images/` endpoint.

The development Deployment mounts a separate serving certificate for port 8444.
For an externally reachable hostname, configure a matching certificate or a
trusted HTTPS reverse proxy with a secure backend connection. The callback CA
bundle must trust the certificate presented at `PREPARATION_BASE_URL`.

The default NetworkPolicy does not open port 8444 outside the operator namespace.
Add an installation-specific policy for the preparation network or its trusted
proxy. Keep the internal image listener private; its configuration media carries
per-attempt credentials. Do not expose the internal image Service through the
callback proxy.

The existing HTTPS egress policy permits port 443. A source ISO on another port
needs an explicit egress rule for that server and port. Network configuration
must also permit the live server to reach its image registries and callback
endpoint. Hub-side checks cannot prove the later live-server network path.

The manager caches verified boot ISOs under `DATA_DIR/preparation`. Allow
storage for one ISO per active attempt, up to 20 GiB per ISO. A pod replacement
can lose this cache; the manager then fetches and verifies the same artifact
again. Retain the source ISO until the workflow and its cleanup have completed.

## Prepare the reusable live ISO

The ISO needs generic networking and callback units. Existing ISOs do not gain
this behavior from a configuration DataImage. The guest must already contain
`jq`, `curl`, `nmstatectl`, `nm-online`, `mount` and `udevadm`; preparation
does not download tools to establish its initial network.

For development, extend a complete installer-generated Ignition and embed it
into a new ISO. Run these commands from the IBI-O repository and supply
`SOURCE_ISO`:

```bash
umask 077
run_dir=$(mktemp -d /tmp/ibi-preparation.XXXXXX)
coreos-installer iso ignition show "$SOURCE_ISO" > "$run_dir/original.ign"
go run ./cmd/preparation-ignition < "$run_dir/original.ign" \
  > "$run_dir/preparation.ign"
coreos-installer iso ignition embed --force \
  --ignition-file "$run_dir/preparation.ign" \
  --output "$run_dir/rhcos-ibi-preparation.iso" "$SOURCE_ISO"
sha256sum "$run_dir/rhcos-ibi-preparation.iso"
```

Publish the new ISO and put its URL and digest in the ImageClusterInstall.
The utility preserves existing SSH keys and preparation commands. It requires
the disk-preparation service to use `Type=oneshot`, including its drop-ins, so
success cannot be reported while disk preparation is still running. It adds:

- A required bootstrap unit before `install-rhcos-and-restore-seed.service`.
- A guest script that reads the `cluster-config` filesystem, applies the existing
  `raw_nm_state_config` payload, and checks networking before disk preparation.
- A success callback after the preparation service finishes and a failure
  callback unit for service failures.

The bootstrap fails before disk preparation if required configuration is absent
or invalid. With no per-host nmstate content, the environment must provide working
networking, for example DHCP. Callback retries are bounded by the attempt deadline.
SSH is available for diagnostics using the key already embedded in the ISO;
this callback workflow does not require the matching private SSH key in a Secret.

## State, recovery and cleanup

The controller records attempt identity, target identity, deadlines, callback
result and progress in `status.preProvisioning`. It freezes installation spec
changes when preparation starts. Normal installation timing is not started until
the preparation handoff completes.

Each attempt has fresh credentials and an immutable Secret containing its
configuration ISO. The Secret allows the exact same configuration bytes to be
served after a manager or server restart. Only a token hash appears in status.
The payload uses the existing `cluster-config` label and network-manifest layout;
it contains preparation settings rather than the final cluster credentials.

A successful callback does not immediately start disk boot. The controller:

1. Revokes the attempt token and requests power-off while retaining media.
2. Waits for actual power-off, then clears the live ISO request.
3. Waits for BMO deployment teardown to finish with cleaning disabled.
4. Deletes the preparation DataImage and waits for its finalizer to complete.
5. Removes the preparation artifacts and returns the host to externally
   provisioned state, offline and without a boot image.
6. Allows the normal IBI flow to generate a fresh cluster-configuration DataImage
   and request disk boot.

Timeout, failure and deletion use the same teardown barrier. A retry starts only
after the previous deployment and media are released, with a new attempt ID and
token. A final failure keeps normal installation gated. Cleanup errors retain
ownership and finalizers rather than claiming that an in-use server is released.
BMO owns the `baremetalhost.metal3.io/live-iso-dataimage` and
`dataimage.metal3.io/deployment-owner` annotations. Preserve these keys in
GitOps and other automation; do not edit them to request or cancel preparation.
The DataImage receives an explicit release acknowledgement only after deployment
teardown succeeds. Missing or malformed ownership metadata is not treated as
evidence that configuration is optional or that cleanup has completed.

O-Cloud Manager integration must wait for preparation finalization before
releasing the allocation. The `deploymentReleased` status field records the
media-teardown checkpoint while artifact cleanup can still be in progress. That
integration and supported ACM deployment configuration are separate from
exercising this workflow directly through an ICI.

## Validation before platform delivery

Unit and workflow tests cover input validation, immutable payloads, token/target
binding, replay rejection, checksum/TLS verification, restart recovery, deadlines,
retries and cleanup ordering. They do not qualify BMC firmware or actual disk
preservation.

The next hardware test must cover concurrent media on first boot, static network
bootstrap, actual seed restoration, success and failure callbacks, restart during
preparation, cancellation, and handoff into normal IBI installation. Confirm that
teardown does not clean the prepared disk, no attempt artifacts remain after
cleanup, and ordinary IBI installation still works without preparation enabled.
