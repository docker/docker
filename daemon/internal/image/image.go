package image

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	imageConfigJSONKey          = "config"
	imageContainerConfigJSONKey = "container_config"
	exposedPortsJSONKey         = "ExposedPorts"
	jsonNullValue               = "null"
)

// ID is the content-addressable ID of an image.
type ID digest.Digest

func (id ID) String() string {
	return id.Digest().String()
}

// Digest converts ID into a digest
func (id ID) Digest() digest.Digest {
	return digest.Digest(id)
}

// V1Image stores the V1 image configuration.
type V1Image struct {
	// ID is the legacy V1 image ID, encoded as a 64-character hexadecimal
	// string without the digest algorithm.
	ID string `json:"id,omitempty"`

	// Parent is the legacy V1 ID of the parent image, encoded without the
	// digest algorithm.
	//
	// Depending on how the image was created, this field may be empty and
	// is only set for images that were built/created locally. This field
	// is empty if the image was pulled from an image registry.
	Parent string `json:"parent,omitempty"`

	// Comment is an optional message that can be set when committing or
	// importing the image.
	Comment string `json:"comment,omitempty"`

	// Created is the timestamp at which the image was created
	Created *time.Time `json:"created"`

	// Container is the ID of the container that was used to create the image.
	//
	// Depending on how the image was created, this field may be empty.
	Container string `json:"container,omitempty"`

	// ContainerConfig is the configuration of the container that was committed
	// into the image.
	ContainerConfig container.Config `json:"container_config"`

	// DockerVersion is the version of Docker that was used to build the image.
	//
	// Depending on how the image was created, this field may be empty.
	DockerVersion string `json:"docker_version,omitempty"`

	// Author is the name of the author that was specified when committing the
	// image, or as specified through MAINTAINER (deprecated) in the Dockerfile.
	Author string `json:"author,omitempty"`

	// Config is the configuration of the container received from the client.
	Config *container.Config `json:"config,omitempty"`

	// Architecture is the hardware CPU architecture that the image runs on.
	Architecture string `json:"architecture,omitempty"`

	// Variant is the CPU architecture variant (presently ARM-only).
	Variant string `json:"variant,omitempty"`

	// OS is the Operating System the image is built to run on.
	OS string `json:"os,omitempty"`

	// Size is the total size of the image including all layers it is composed of.
	Size int64 `json:",omitempty"`
}

// Image stores the image configuration
type Image struct {
	V1Image

	// Parent is the content-addressable ID of the parent image.
	//
	// This field intentionally shadows V1Image.Parent: V1Image.Parent stores
	// the legacy V1 ID without its digest algorithm, whereas Parent stores the
	// full content-addressable image ID.
	//
	// Depending on how the image was created, this field may be empty and
	// is only set for images that were built/created locally. This field
	// is empty if the image was pulled from an image registry.
	Parent ID `json:"parent,omitempty"`

	// RootFS contains information about the image's RootFS, including the
	// layer IDs.
	RootFS  *RootFS   `json:"rootfs,omitempty"`
	History []History `json:"history,omitempty"`

	// OsVersion is the version of the Operating System the image is built to
	// run on (especially for Windows).
	OSVersion  string   `json:"os.version,omitempty"`
	OSFeatures []string `json:"os.features,omitempty"`

	// rawJSON caches the immutable JSON associated with this image.
	rawJSON []byte

	// computedID is the ID computed from the hash of the image config.
	// Not to be confused with the legacy V1 ID in V1Image.
	computedID ID

	// Details holds additional details about image
	Details *Details `json:"-"`
}

// Details provides additional image data
type Details struct {
	// ManifestDescriptor is the descriptor of the platform-specific manifest
	// chosen by the [GetImage] call that returned this image.
	// The exact descriptor depends on the [GetImageOpts.Platform] field
	// passed to [GetImage] and the content availability.
	// This is only set by the containerd image service.
	ManifestDescriptor *ocispec.Descriptor
}

// RawJSON returns the immutable JSON associated with the image.
func (img *Image) RawJSON() []byte {
	return img.rawJSON
}

// ID returns the image's content-addressable ID.
func (img *Image) ID() ID {
	return img.computedID
}

// ImageID stringifies ID.
func (img *Image) ImageID() string {
	return img.ID().String()
}

// RunConfig returns the image's container config.
func (img *Image) RunConfig() *container.Config {
	return img.Config
}

// BaseImgArch returns the image's architecture. If not populated, defaults to the host runtime arch.
func (img *Image) BaseImgArch() string {
	arch := img.Architecture
	if arch == "" {
		arch = runtime.GOARCH
	}
	return arch
}

// BaseImgVariant returns the image's variant, whether populated or not.
// This avoids creating an inconsistency where the stored image variant
// is "greater than" (i.e. v8 vs v6) the actual image variant.
func (img *Image) BaseImgVariant() string {
	return img.Variant
}

// OperatingSystem returns the image's operating system. If not populated, defaults to the host runtime OS.
func (img *Image) OperatingSystem() string {
	os := img.OS
	if os == "" {
		os = runtime.GOOS
	}
	return os
}

// Platform generates an OCI platform from the image
func (img *Image) Platform() ocispec.Platform {
	return ocispec.Platform{
		Architecture: img.Architecture,
		OS:           img.OS,
		OSVersion:    img.OSVersion,
		OSFeatures:   img.OSFeatures,
		Variant:      img.Variant,
	}
}

// MarshalJSON serializes the image to JSON. It sorts the top-level keys so
// that JSON that's been manipulated by a push/pull cycle with a legacy
// registry won't end up with a different key order.
func (img *Image) MarshalJSON() ([]byte, error) {
	type MarshalImage Image

	pass1, err := json.Marshal(MarshalImage(*img))
	if err != nil {
		return nil, err
	}

	var c map[string]*json.RawMessage
	if err := json.Unmarshal(pass1, &c); err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// ChildConfig is the configuration to apply to an Image to create a new
// Child image. Other properties of the image are copied from the parent.
type ChildConfig struct {
	ContainerID     string
	Author          string
	Comment         string
	DiffID          layer.DiffID
	ContainerConfig *container.Config
	Config          *container.Config
}

// NewImage creates a new image with the given ID
func NewImage(id ID) *Image {
	return &Image{
		computedID: id,
	}
}

// NewChildImage creates a new Image as a child of this image.
func NewChildImage(img *Image, child ChildConfig, os string) *Image {
	isEmptyLayer := layer.IsEmpty(child.DiffID)
	var rootFS *RootFS
	if img.RootFS != nil {
		rootFS = img.RootFS.Clone()
	} else {
		rootFS = NewRootFS()
	}

	if !isEmptyLayer {
		rootFS.Append(child.DiffID)
	}
	imgHistory := NewHistory(
		child.Author,
		child.Comment,
		strings.Join(child.ContainerConfig.Cmd, " "),
		isEmptyLayer)

	return &Image{
		V1Image: V1Image{
			Config:          child.Config,
			Architecture:    img.BaseImgArch(),
			Variant:         img.BaseImgVariant(),
			OS:              os,
			Container:       child.ContainerID,
			ContainerConfig: *child.ContainerConfig,
			Author:          child.Author,
			Created:         imgHistory.Created,
		},
		RootFS:     rootFS,
		History:    append(img.History, imgHistory),
		OSFeatures: img.OSFeatures,
		OSVersion:  img.OSVersion,
	}
}

// Clone clones an image and changes ID.
func Clone(base *Image, id ID) *Image {
	img := *base
	img.RootFS = img.RootFS.Clone()
	img.V1Image.ID = id.String()
	img.computedID = id
	return &img
}

// History stores build commands that were used to create an image
type History = ocispec.History

// NewHistory creates a new history struct from arguments, and sets the created
// time to the current time in UTC
func NewHistory(author, comment, createdBy string, isEmptyLayer bool) History {
	now := time.Now().UTC()
	return History{
		Author:     author,
		Created:    &now,
		CreatedBy:  createdBy,
		Comment:    comment,
		EmptyLayer: isEmptyLayer,
	}
}

// Exporter provides interface for loading and saving images
type Exporter interface {
	Load(context.Context, io.ReadCloser, io.Writer, bool) error
	// TODO: Load(net.Context, io.ReadCloser, <- chan StatusMessage) error
	Save(context.Context, []string, io.Writer) error
}

// NewFromJSON creates an Image configuration from json.
func NewFromJSON(src []byte) (*Image, error) {
	img := &Image{}

	normalized, err := normalizeConfigExposedPortRanges(src)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(normalized, img); err != nil {
		return nil, err
	}
	if img.RootFS == nil {
		return nil, errors.New("invalid image JSON, no RootFS key")
	}

	img.rawJSON = src

	return img, nil
}

func normalizeConfigExposedPortRanges(src []byte) ([]byte, error) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal(src, &config); err != nil {
		return nil, err
	}

	changed := false
	for _, key := range []string{imageConfigJSONKey, imageContainerConfigJSONKey} {
		normalized, ok, err := normalizeExposedPortRanges(config[key])
		if err != nil {
			return nil, err
		}
		if ok {
			config[key] = normalized
			changed = true
		}
	}

	if !changed {
		return src, nil
	}
	return json.Marshal(config)
}

func normalizeExposedPortRanges(src json.RawMessage) (json.RawMessage, bool, error) {
	if len(src) == 0 || string(src) == jsonNullValue {
		return nil, false, nil
	}

	var config map[string]json.RawMessage
	if err := json.Unmarshal(src, &config); err != nil {
		return nil, false, err
	}

	exposedPortsJSON, ok := config[exposedPortsJSONKey]
	if !ok || string(exposedPortsJSON) == jsonNullValue {
		return nil, false, nil
	}

	var exposedPorts map[string]json.RawMessage
	if err := json.Unmarshal(exposedPortsJSON, &exposedPorts); err != nil {
		return nil, false, err
	}

	normalizedPorts := make(map[string]json.RawMessage, len(exposedPorts))
	changed := false
	for port, value := range exposedPorts {
		if parsedPort, err := network.ParsePort(port); err == nil {
			normalizedPorts[parsedPort.String()] = value
			if parsedPort.String() != port {
				changed = true
			}
			continue
		}

		portRange, err := network.ParsePortRange(port)
		if err != nil {
			normalizedPorts[port] = value
			continue
		}
		changed = true
		for p := range portRange.All() {
			normalizedPorts[p.String()] = value
		}
	}

	if !changed {
		return nil, false, nil
	}

	normalizedExposedPorts, err := json.Marshal(normalizedPorts)
	if err != nil {
		return nil, false, err
	}
	config[exposedPortsJSONKey] = normalizedExposedPorts

	normalizedConfig, err := json.Marshal(config)
	if err != nil {
		return nil, false, err
	}
	return normalizedConfig, true, nil
}
