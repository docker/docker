package metadata

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/opencontainers/go-digest"
)

// V2MetadataService maps layer IDs to a set of known metadata for
// the layer.
type V2MetadataService interface {
	GetMetadata(diffID layer.DiffID) ([]V2Metadata, error)
	GetDiffID(dgst digest.Digest) (layer.DiffID, error)
	Add(diffID layer.DiffID, metadata V2Metadata) error
	TagAndAdd(diffID layer.DiffID, hmacKey []byte, metadata V2Metadata) error
	Remove(metadata V2Metadata) error
}

// v2MetadataService implements V2MetadataService
type v2MetadataService struct {
	store Store
}

var _ V2MetadataService = &v2MetadataService{}

// V2Metadata contains information about a compressed layer blob.
type V2Metadata struct {
	Digest           digest.Digest
	SourceRepository string
	MediaType        string `json:",omitempty"`
	// HMAC hashes above attributes with recent authconfig digest used as a key in order to determine matching
	// metadata entries accompanied by the same credentials without actually exposing them.
	HMAC string
}

// CheckV2MetadataHMAC returns true if the given "meta" is tagged with a hmac hashed by the given "key".
func CheckV2MetadataHMAC(meta *V2Metadata, key []byte) bool {
	if meta.HMAC == "" || len(key) == 0 {
		return meta.HMAC == "" && len(key) == 0
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(meta.Digest))
	mac.Write([]byte(meta.SourceRepository))
	if meta.MediaType != "" {
		mac.Write([]byte{0})
		mac.Write([]byte(meta.MediaType))
	}
	expectedMac := mac.Sum(nil)

	storedMac, err := hex.DecodeString(meta.HMAC)
	if err != nil {
		return false
	}

	return hmac.Equal(storedMac, expectedMac)
}

// ComputeV2MetadataHMAC returns a hmac for the given "meta" hash by the given key.
func ComputeV2MetadataHMAC(key []byte, meta *V2Metadata) string {
	if len(key) == 0 || meta == nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(meta.Digest))
	mac.Write([]byte(meta.SourceRepository))
	if meta.MediaType != "" {
		mac.Write([]byte{0})
		mac.Write([]byte(meta.MediaType))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// ComputeV2MetadataHMACKey returns a key for the given "authConfig" that can be used to hash v2 metadata
// entries.
func ComputeV2MetadataHMACKey(authConfig *registry.AuthConfig) ([]byte, error) {
	if authConfig == nil {
		return nil, nil
	}
	buf, err := json.Marshal(&authConfigKeyInput{
		Username:      authConfig.Username,
		Password:      authConfig.Password, // #nosec G117 -- included only as input to the derived digest below.
		Auth:          authConfig.Auth,
		IdentityToken: authConfig.IdentityToken,
		RegistryToken: authConfig.RegistryToken,
	})
	if err != nil {
		return nil, err
	}
	return []byte(digest.FromBytes(buf)), nil
}

// authConfigKeyInput is a reduced AuthConfig structure holding just relevant credential data eligible for
// hmac key creation.
type authConfigKeyInput struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`

	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

// maxMetadata is the number of metadata entries to keep per layer DiffID.
const maxMetadata = 50

// NewV2MetadataService creates a new diff ID to v2 metadata mapping service.
func NewV2MetadataService(store Store) V2MetadataService {
	return &v2MetadataService{
		store: store,
	}
}

func (serv *v2MetadataService) diffIDNamespace() string {
	return "v2metadata-by-diffid"
}

func (serv *v2MetadataService) digestNamespace() string {
	return "diffid-by-digest"
}

func (serv *v2MetadataService) diffIDKey(diffID layer.DiffID) string {
	return string(diffID.Algorithm()) + "/" + diffID.Encoded()
}

func (serv *v2MetadataService) digestKey(dgst digest.Digest) string {
	return string(dgst.Algorithm()) + "/" + dgst.Encoded()
}

// GetMetadata finds the metadata associated with a layer DiffID.
func (serv *v2MetadataService) GetMetadata(diffID layer.DiffID) ([]V2Metadata, error) {
	if serv.store == nil {
		return nil, errors.New("no metadata storage")
	}
	jsonBytes, err := serv.store.Get(serv.diffIDNamespace(), serv.diffIDKey(diffID))
	if err != nil {
		return nil, err
	}

	var metadata []V2Metadata
	if err := json.Unmarshal(jsonBytes, &metadata); err != nil {
		return nil, err
	}

	return metadata, nil
}

// GetDiffID finds a layer DiffID from a digest.
func (serv *v2MetadataService) GetDiffID(dgst digest.Digest) (layer.DiffID, error) {
	if serv.store == nil {
		return "", errors.New("no metadata storage")
	}
	diffIDBytes, err := serv.store.Get(serv.digestNamespace(), serv.digestKey(dgst))
	if err != nil {
		return "", err
	}

	return layer.DiffID(diffIDBytes), nil
}

// Add associates metadata with a layer DiffID. If too many metadata entries are
// present, the oldest one is dropped.
func (serv *v2MetadataService) Add(diffID layer.DiffID, metadata V2Metadata) error {
	if serv.store == nil {
		// Support a service which has no backend storage, in this case
		// an add becomes a no-op.
		// TODO: implement in memory storage
		return nil
	}
	oldMetadata, err := serv.GetMetadata(diffID)
	if err != nil {
		oldMetadata = nil
	}
	newMetadata := make([]V2Metadata, 0, len(oldMetadata)+1)

	// Copy all other metadata to new slice
	for _, oldMeta := range oldMetadata {
		if oldMeta != metadata {
			newMetadata = append(newMetadata, oldMeta)
		}
	}

	newMetadata = append(newMetadata, metadata)

	if len(newMetadata) > maxMetadata {
		newMetadata = newMetadata[len(newMetadata)-maxMetadata:]
	}

	jsonBytes, err := json.Marshal(newMetadata)
	if err != nil {
		return err
	}

	err = serv.store.Set(serv.diffIDNamespace(), serv.diffIDKey(diffID), jsonBytes)
	if err != nil {
		return err
	}

	return serv.store.Set(serv.digestNamespace(), serv.digestKey(metadata.Digest), []byte(diffID))
}

// TagAndAdd amends the given "meta" for hmac hashed by the given "hmacKey" and associates it with a layer
// DiffID. If too many metadata entries are present, the oldest one is dropped.
func (serv *v2MetadataService) TagAndAdd(diffID layer.DiffID, hmacKey []byte, meta V2Metadata) error {
	meta.HMAC = ComputeV2MetadataHMAC(hmacKey, &meta)
	return serv.Add(diffID, meta)
}

// Remove disassociates a metadata entry from a layer DiffID.
func (serv *v2MetadataService) Remove(metadata V2Metadata) error {
	if serv.store == nil {
		// Support a service which has no backend storage, in this case
		// an remove becomes a no-op.
		// TODO: implement in memory storage
		return nil
	}
	diffID, err := serv.GetDiffID(metadata.Digest)
	if err != nil {
		return err
	}
	oldMetadata, err := serv.GetMetadata(diffID)
	if err != nil {
		oldMetadata = nil
	}
	newMetadata := make([]V2Metadata, 0, len(oldMetadata))

	// Copy all other metadata to new slice
	for _, oldMeta := range oldMetadata {
		if oldMeta != metadata {
			newMetadata = append(newMetadata, oldMeta)
		}
	}

	if len(newMetadata) == 0 {
		return serv.store.Delete(serv.diffIDNamespace(), serv.diffIDKey(diffID))
	}

	jsonBytes, err := json.Marshal(newMetadata)
	if err != nil {
		return err
	}

	return serv.store.Set(serv.diffIDNamespace(), serv.diffIDKey(diffID), jsonBytes)
}
