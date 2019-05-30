// +build amd64 arm64 386

package etw

import (
	"bytes"
	"encoding/binary"
	"unsafe"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"
)

// NewProviderWithID creates and registers a new ETW provider, allowing the
// provider ID to be manually specified. This is most useful when there is an
// existing provider ID that must be used to conform to existing diagnostic
// infrastructure.
func NewProviderWithID(name string, id *guid.GUID, callback EnableCallback) (provider *Provider, err error) {
	providerCallbackOnce.Do(func() {
		globalProviderCallback = windows.NewCallback(providerCallbackAdapter)
	})

	provider = providers.newProvider()
	defer func(provider *Provider) {
		if err != nil {
			providers.removeProvider(provider)
		}
	}(provider)
	provider.ID = id
	provider.callback = callback

	if err := eventRegister((*windows.GUID)(provider.ID), globalProviderCallback, uintptr(provider.index), &provider.handle); err != nil {
		return nil, err
	}

	metadata := &bytes.Buffer{}
	binary.Write(metadata, binary.LittleEndian, uint16(0)) // Write empty size for buffer (to update later)
	metadata.WriteString(name)
	metadata.WriteByte(0)                                                   // Null terminator for name
	binary.LittleEndian.PutUint16(metadata.Bytes(), uint16(metadata.Len())) // Update the size at the beginning of the buffer
	provider.metadata = metadata.Bytes()

	if err := eventSetInformation(
		provider.handle,
		eventInfoClassProviderSetTraits,
		uintptr(unsafe.Pointer(&provider.metadata[0])),
		uint32(len(provider.metadata))); err != nil {

		return nil, err
	}

	return provider, nil
}
