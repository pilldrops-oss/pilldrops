package artifact

import (
	"encoding/json"
	"fmt"
)

// Metadata is the contents of `<buildID>/metadata.json`: the boot-compatibility
// record a node reads to decide whether it can run a build. It is the source
// of truth for the version triple; template_builds carries a copy.
type Metadata struct {
	BuildID    string `json:"build_id"`
	TemplateID string `json:"template_id"`

	// The version triple. Empty means unconstrained.
	KernelVersion      string `json:"kernel_version,omitempty"`
	FirecrackerVersion string `json:"firecracker_version,omitempty"`
	EnvdVersion        string `json:"envd_version,omitempty"`

	// Payload names the build's data object, NameRootfs or NameImage, or
	// PayloadSnapshot for the three-object snapshot layout. Empty means
	// NameRootfs so builds published before custom images keep their meaning.
	Payload string `json:"payload,omitempty"`

	// Snapshot is present only on a payload:"snapshot" build. It is what a node
	// needs to restore before it has read anything from the database — the
	// database is the control plane's, and the node role sees one table of it.
	Snapshot *SnapshotMeta `json:"snapshot,omitempty"`

	// RootfsSize and RootfsSHA256 restate the rootfs header so the index can
	// be rebuilt from the store without parsing binary headers.
	RootfsSize   int64  `json:"rootfs_size"`
	RootfsSHA256 string `json:"rootfs_sha256"`

	CreatedUnix int64 `json:"created_unix"`
}

// SnapshotMeta describes a captured machine: what it was captured from and the
// network identity baked into its memory image. A restore reads it instead of
// the pause snapshot's own meta file, which stays on the node it was taken on.
type SnapshotMeta struct {
	// Runtime the capture was taken on. A memory image restores only there.
	Runtime string `json:"runtime"`
	// Template is the source sandbox's template. Its rootfs is the backing file
	// the overlay is a delta against, so the node needs it as well as the three
	// snapshot objects.
	Template string `json:"template"`
	// VCPU and MemMB are fixed: Firecracker restores into the same shape it
	// snapshotted, so a create naming a different size is refused rather than
	// silently given this one.
	VCPU  int `json:"vcpu"`
	MemMB int `json:"mem_mb"`
	// SourceSandbox is provenance, not a dependency; it may be long destroyed.
	SourceSandbox string `json:"source_sandbox,omitempty"`
	// TapName and VMIP are baked into the memory image and cannot be changed by
	// the restore, which is why a child boots in its own network namespace.
	//
	// They are here to be *read by an operator*, not by the restore: the restore
	// takes them from NameSnapMeta, the capture engine's own record, which is
	// the only copy that can be trusted to match the memory image byte for byte.
	// Do not add a third consumer of these two fields.
	TapName string `json:"tap_name,omitempty"`
	VMIP    string `json:"vm_ip,omitempty"`
}

// MaxMetadataBytes bounds a read of metadata.json.
const MaxMetadataBytes = 64 << 10

// Marshal encodes the record, indented for operators reading it directly.
func (m Metadata) Marshal() ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

// ParseMetadata decodes a metadata.json object. Unknown fields are allowed so
// a newer publisher does not break older nodes.
func ParseMetadata(raw []byte) (Metadata, error) {
	if len(raw) > MaxMetadataBytes {
		return Metadata{}, fmt.Errorf("artifact: metadata.json is %d bytes, over the %d-byte ceiling",
			len(raw), MaxMetadataBytes)
	}
	var m Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return Metadata{}, fmt.Errorf("artifact: metadata.json is not valid JSON: %w", err)
	}
	if m.BuildID == "" {
		return Metadata{}, fmt.Errorf("artifact: metadata.json names no build")
	}
	return m, nil
}

// PayloadName is Payload with the pre-custom-image default applied.
func (m Metadata) PayloadName() string {
	if m.Payload == "" {
		return NameRootfs
	}
	return m.Payload
}
