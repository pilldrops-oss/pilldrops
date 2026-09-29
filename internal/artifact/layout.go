package artifact

import (
	"fmt"
	"strings"
)

// The key scheme. Everything for one build sits under its ID:
//
//	<buildID>/rootfs           root filesystem image (Firecracker)
//	<buildID>/rootfs.header    its block map
//	<buildID>/image.tar        Docker image tarball (Docker runtime)
//	<buildID>/image.tar.header its block map
//	<buildID>/metadata.json    what a node needs to boot it
//	<buildID>/memfile          guest memory       ┐
//	<buildID>/snapfile         Firecracker state  ├ a named snapshot, payload "snapshot"
//	<buildID>/overlay          the overlay disk   ┘
//
// metadata.json's `payload` field says which data object a build carries.
// "snapshot" is the one payload that names three: a restore needs memory,
// machine state and disk together, and a build holding two of them is broken.

const (
	// NameRootfs is the root filesystem image.
	NameRootfs = "rootfs"
	// NameMetadata is the boot-compatibility record a node reads without touching the database.
	NameMetadata = "metadata.json"

	// NameImage is a Docker image tarball (`docker save` output). It is a
	// separate name from NameRootfs so a KVM node cannot link a tarball in
	// where a squashfs belongs.
	NameImage = "image.tar"

	// A named snapshot's data objects: guest memory, Firecracker VM state, the
	// source sandbox's overlay disk, and the engine's own capture record. All
	// four are needed to restore, so a build carrying one carries all of them.
	NameMemfile  = "memfile"
	NameSnapfile = "snapfile"
	NameOverlay  = "overlay"
	// NameSnapMeta is the capture engine's record, opaque to every tier between
	// the two ends: the node that captures writes it and the node that restores
	// reads it, and nothing in between parses it. That is what keeps the guest's
	// baked network identity out of the artifact and daemon layers, which have
	// no business knowing a MAC or a tap name.
	NameSnapMeta = "snapmeta"

	// HeaderSuffix is appended to a data object's name to get its block map.
	HeaderSuffix = ".header"

	// PayloadSnapshot is the metadata.json payload of a named snapshot. Unlike
	// the other payloads it is not an object name: see SnapshotObjects.
	PayloadSnapshot = "snapshot"
)

// SnapshotObjects are the data objects a payload:"snapshot" build carries, in
// the order a fetch should materialise them. One list, so publish, fetch and
// delete cannot disagree about what a snapshot is made of.
//
// NameRootfs is in the list because a capture is only restorable against the
// exact read-only image it was taken against: the overlay is a delta on it and
// the memory image refers into it. Every node builds its own from the
// Dockerfile, so the template NAME resolves to different bytes on every node
// and a restore against the local build comes up with a guest whose binaries
// read EIO. The image travels with
// the capture, like everything else the capture needs.
//
// The order is the order a node links them into a capture directory, and the
// root image must stay last: a directory holding it is a template the create
// path boots, so it appearing early lets a create boot a half-landed capture.
func SnapshotObjects() []string {
	return []string{NameSnapfile, NameMemfile, NameOverlay, NameSnapMeta, NameRootfs}
}

// Key returns the object key for one part of a build. The build ID is
// validated on its own since it may derive from a caller-supplied name.
func Key(buildID, name string) (string, error) {
	if err := ValidateBuildID(buildID); err != nil {
		return "", err
	}
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: artifact name %q must be a single path segment", ErrInvalidKey, name)
	}
	key := buildID + "/" + name
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// HeaderKey returns the key of the block map describing a data object.
func HeaderKey(buildID, name string) (string, error) {
	return Key(buildID, name+HeaderSuffix)
}

// BuildIDOf returns the build a key belongs to.
func BuildIDOf(key string) (string, bool) {
	id, _, found := strings.Cut(key, "/")
	if !found || id == "" {
		return "", false
	}
	return id, true
}

// TemplateIDForImageTag maps a Docker image tag to the template id its build
// is indexed under; publish and create must share this definition. ok is
// false for a shape publish could never have written.
//
//	sf-acme-mytool:latest  ->  sf-acme-mytool, true
//	python:3.11            ->  python, true
//	img@sha256:…           ->  "", false
func TemplateIDForImageTag(tag string) (string, bool) {
	id := tag
	if id == "" || strings.Contains(id, "@") {
		return "", false
	}
	// Checked before the '/' to '_' rewrite, which would make "../escape" look legal.
	if strings.Contains(id, "..") {
		return "", false
	}
	// Cut the version from the last segment only, so a registry port is not taken for one.
	if slash := strings.LastIndex(id, "/"); slash >= 0 {
		if colon := strings.LastIndex(id[slash+1:], ":"); colon >= 0 {
			id = id[:slash+1+colon]
		}
	} else if colon := strings.LastIndex(id, ":"); colon >= 0 {
		id = id[:colon]
	}
	id = strings.ReplaceAll(id, "/", "_")
	if id == "" || len(id) > 128 {
		return "", false
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return "", false
		}
	}
	return id, true
}

// ValidateBuildID accepts letters, digits, - and _ up to 128 bytes. Build IDs
// are machine-generated, so anything unusual is a bug or an attack.
func ValidateBuildID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty build id", ErrInvalidKey)
	case len(id) > 128:
		return fmt.Errorf("%w: build id longer than 128 bytes", ErrInvalidKey)
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("%w: build id %q contains %q; only letters, digits, - and _ are allowed",
				ErrInvalidKey, id, r)
		}
	}
	return nil
}
