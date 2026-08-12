#!/usr/bin/env bash

release_oci_probe_index() {
  [[ $# -eq 2 ]] || return 2
  local die_function="$1" ref="$2" inspection_file error_file values status
  RELEASE_OCI_INDEX_EXISTS=0
  RELEASE_OCI_INDEX_DIGEST=
  RELEASE_OCI_PLATFORM_MANIFEST_DIGEST=
  RELEASE_OCI_PLATFORM_CONFIG_DIGEST=
  RELEASE_OCI_ATTESTATION_MANIFEST_DIGEST=

  inspection_file="$(mktemp "${TMPDIR:-/tmp}/dirextalk-oci-index.XXXXXX")"
  error_file="$(mktemp "${TMPDIR:-/tmp}/dirextalk-oci-error.XXXXXX")"
  if docker buildx imagetools inspect "$ref" --format '{{json .}}' >"$inspection_file" 2>"$error_file"; then
    status=0
  else
    status=$?
  fi
  if [[ "$status" -ne 0 ]]; then
    if grep -Fqx "ERROR: docker.io/$ref: not found" "$error_file" || \
       grep -Fqx "ERROR: $ref: not found" "$error_file"; then
      rm -f "$inspection_file" "$error_file"
      return
    fi
    rm -f "$inspection_file" "$error_file"
    "$die_function" "could not inspect remote OCI index: $ref"
  fi

  if values="$(python3 - "$inspection_file" "$ref" <<'PY'
import json, pathlib, re, sys

path, ref = sys.argv[1:]
try:
    value = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
except Exception as exc:
    raise SystemExit(f"invalid imagetools response for {ref}: {exc}")

manifest = value.get("manifest") if isinstance(value, dict) else None
if not isinstance(manifest, dict):
    raise SystemExit(f"imagetools response for {ref} has no manifest")
if manifest.get("mediaType") != "application/vnd.oci.image.index.v1+json":
    raise SystemExit(f"remote image is not an OCI index: {ref}")

digest_re = re.compile(r"sha256:[0-9a-f]{64}")
digest = manifest.get("digest")
if not isinstance(digest, str) or not digest_re.fullmatch(digest):
    raise SystemExit(f"remote OCI index has an invalid digest: {ref}")

descriptors = manifest.get("manifests")
if not isinstance(descriptors, list):
    raise SystemExit(f"remote OCI index has no platform manifests: {ref}")

platforms = []
platform_digests = []
attestations = []
for descriptor in descriptors:
    if not isinstance(descriptor, dict):
        raise SystemExit(f"remote OCI index has an invalid descriptor: {ref}")
    if descriptor.get("mediaType") != "application/vnd.oci.image.manifest.v1+json":
        raise SystemExit(f"remote OCI index has a non-OCI manifest descriptor: {ref}")
    descriptor_digest = descriptor.get("digest")
    if not isinstance(descriptor_digest, str) or not digest_re.fullmatch(descriptor_digest):
        raise SystemExit(f"remote OCI index has an invalid descriptor digest: {ref}")
    platform = descriptor.get("platform")
    if not isinstance(platform, dict):
        raise SystemExit(f"remote OCI index descriptor has no platform: {ref}")
    os_name = platform.get("os")
    architecture = platform.get("architecture")
    if os_name == "unknown" and architecture == "unknown":
        annotations = descriptor.get("annotations")
        if not isinstance(annotations, dict) or annotations.get("vnd.docker.reference.type") != "attestation-manifest":
            raise SystemExit(f"remote OCI index has an unexpected unknown platform descriptor: {ref}")
        subject = annotations.get("vnd.docker.reference.digest")
        if not isinstance(subject, str) or not digest_re.fullmatch(subject):
            raise SystemExit(f"remote OCI index has an unbound attestation descriptor: {ref}")
        attestations.append((descriptor_digest, subject))
        continue
    platforms.append((os_name, architecture, platform.get("variant")))
    platform_digests.append(descriptor_digest)

if platforms != [("linux", "amd64", None)]:
    raise SystemExit(f"remote OCI index must contain exactly linux/amd64: {ref}")
if len(attestations) != 1 or attestations[0][1] != platform_digests[0]:
    raise SystemExit(f"remote OCI index must contain one linux/amd64-bound attestation manifest: {ref}")
print(digest)
print(platform_digests[0])
print(attestations[0][0])
PY
)"; then
    status=0
  else
    status=$?
  fi
  rm -f "$inspection_file" "$error_file"
  [[ "$status" -eq 0 ]] || "$die_function" "remote OCI index verification failed: $ref"
  mapfile -t RELEASE_OCI_INDEX_VALUES <<<"$values"
  [[ "${#RELEASE_OCI_INDEX_VALUES[@]}" == 3 ]] || "$die_function" "remote OCI index proof is incomplete: $ref"

  RELEASE_OCI_INDEX_DIGEST="${RELEASE_OCI_INDEX_VALUES[0]}"
  RELEASE_OCI_PLATFORM_MANIFEST_DIGEST="${RELEASE_OCI_INDEX_VALUES[1]}"
  RELEASE_OCI_ATTESTATION_MANIFEST_DIGEST="${RELEASE_OCI_INDEX_VALUES[2]}"
  RELEASE_OCI_PLATFORM_CONFIG_DIGEST="$(release_oci_platform_config_digest \
    "$die_function" "$ref" "$RELEASE_OCI_PLATFORM_MANIFEST_DIGEST")"
  release_oci_verify_attestation \
    "$die_function" "$ref" "$RELEASE_OCI_ATTESTATION_MANIFEST_DIGEST"
  RELEASE_OCI_INDEX_EXISTS=1
  export RELEASE_OCI_INDEX_EXISTS RELEASE_OCI_INDEX_DIGEST
  export RELEASE_OCI_PLATFORM_MANIFEST_DIGEST RELEASE_OCI_PLATFORM_CONFIG_DIGEST
  export RELEASE_OCI_ATTESTATION_MANIFEST_DIGEST
}

release_oci_raw_manifest() {
  [[ $# -eq 3 ]] || return 2
  local die_function="$1" ref="$2" output_file="$3" status
  if docker buildx imagetools inspect "$ref" --raw >"$output_file"; then
    status=0
  else
    status=$?
  fi
  [[ "$status" -eq 0 ]] || "$die_function" "could not inspect immutable OCI manifest: $ref"
}

release_oci_platform_config_digest() {
  [[ $# -eq 3 ]] || return 2
  local die_function="$1" ref="$2" manifest_digest="$3" repository raw_file digest status
  repository="${ref%%@*}"
  repository="${repository%:*}"
  raw_file="$(mktemp "${TMPDIR:-/tmp}/dirextalk-oci-platform.XXXXXX")"
  release_oci_raw_manifest "$die_function" "$repository@$manifest_digest" "$raw_file"
  if digest="$(python3 - "$raw_file" "$ref" <<'PY'
import json, pathlib, re, sys

path, ref = sys.argv[1:]
try:
    manifest = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
except Exception as exc:
    raise SystemExit(f"invalid linux/amd64 manifest for {ref}: {exc}")
if not isinstance(manifest, dict) or manifest.get("mediaType") != "application/vnd.oci.image.manifest.v1+json":
    raise SystemExit(f"linux/amd64 descriptor does not resolve to an OCI manifest: {ref}")
config = manifest.get("config")
if not isinstance(config, dict) or config.get("mediaType") != "application/vnd.oci.image.config.v1+json":
    raise SystemExit(f"linux/amd64 OCI manifest has an invalid config descriptor: {ref}")
digest = config.get("digest")
if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
    raise SystemExit(f"linux/amd64 OCI manifest has an invalid config digest: {ref}")
print(digest)
PY
)"; then
    status=0
  else
    status=$?
  fi
  rm -f "$raw_file"
  [[ "$status" -eq 0 ]] || "$die_function" "linux/amd64 OCI manifest verification failed: $ref"
  printf '%s\n' "$digest"
}

release_oci_verify_attestation() {
  [[ $# -eq 3 ]] || return 2
  local die_function="$1" ref="$2" attestation_digest="$3" repository raw_file status
  repository="${ref%%@*}"
  repository="${repository%:*}"
  raw_file="$(mktemp "${TMPDIR:-/tmp}/dirextalk-oci-attestation.XXXXXX")"
  release_oci_raw_manifest "$die_function" "$repository@$attestation_digest" "$raw_file"
  if python3 - "$raw_file" "$ref" <<'PY'
import json, pathlib, re, sys

path, ref = sys.argv[1:]
try:
    manifest = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
except Exception as exc:
    raise SystemExit(f"invalid attestation manifest for {ref}: {exc}")
if not isinstance(manifest, dict) or manifest.get("mediaType") != "application/vnd.oci.image.manifest.v1+json":
    raise SystemExit(f"attestation descriptor does not resolve to an OCI manifest: {ref}")
layers = manifest.get("layers")
if not isinstance(layers, list):
    raise SystemExit(f"attestation manifest has no layers: {ref}")
predicates = set()
for layer in layers:
    if not isinstance(layer, dict) or layer.get("mediaType") != "application/vnd.in-toto+json":
        raise SystemExit(f"attestation manifest has a non-in-toto layer: {ref}")
    digest = layer.get("digest")
    if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise SystemExit(f"attestation manifest has an invalid layer digest: {ref}")
    annotations = layer.get("annotations")
    predicate = annotations.get("in-toto.io/predicate-type") if isinstance(annotations, dict) else None
    if not isinstance(predicate, str):
        raise SystemExit(f"attestation manifest has an untyped layer: {ref}")
    predicates.add(predicate)
if "https://spdx.dev/Document" not in predicates or not any(
    value in predicates
    for value in ("https://slsa.dev/provenance/v0.2", "https://slsa.dev/provenance/v1")
):
    raise SystemExit(f"attestation manifest lacks provenance or SBOM: {ref}")
PY
  then
    status=0
  else
    status=$?
  fi
  rm -f "$raw_file"
  [[ "$status" -eq 0 ]] || "$die_function" "OCI attestation verification failed: $ref"
}

release_oci_buildx_metadata_digest() {
  [[ $# -eq 3 ]] || return 2
  local die_function="$1" metadata_file="$2" label="$3"
  [[ -f "$metadata_file" ]] || "$die_function" "$label buildx metadata is unavailable"
  python3 - "$metadata_file" "$label" <<'PY'
import json, pathlib, re, sys

path, label = sys.argv[1:]
try:
    value = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
except Exception as exc:
    raise SystemExit(f"invalid {label} buildx metadata: {exc}")
digest = value.get("containerimage.digest") if isinstance(value, dict) else None
if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
    raise SystemExit(f"{label} buildx metadata has no canonical image digest")
print(digest)
PY
}

release_oci_compare_versions() {
  [[ $# -eq 4 ]] || return 2
  local die_function="$1" left="$2" right="$3" label="$4"
  python3 - "$left" "$right" "$label" <<'PY'
import re, sys

left, right, label = sys.argv[1:]
pattern = re.compile(r"^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")
values = []
for value in (left, right):
    match = pattern.fullmatch(value)
    if not match:
        raise SystemExit(f"{label} is not a canonical version")
    values.append(tuple(map(int, match.groups())))
print((values[0] > values[1]) - (values[0] < values[1]))
PY
}
