package etw

import (
	"crypto/sha1"
	"encoding/binary"
	"strings"
	"unicode/utf16"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"
)

// Provider represents an ETW event provider. It is identified by a provider
// name and ID (GUID), which should always have a 1:1 mapping to each other
// (e.g. don't use multiple provider names with the same ID, or vice versa).
type Provider struct {
	ID         *guid.GUID
	handle     providerHandle
	metadata   []byte
	callback   EnableCallback
	index      uint
	enabled    bool
	level      Level
	keywordAny uint64
	keywordAll uint64
}

// String returns the `provider`.ID as a string
func (provider *Provider) String() string {
	if provider == nil {
		return "<nil>"
	}

	return provider.ID.String()
}

type providerHandle uint64

// ProviderState informs the provider EnableCallback what action is being
// performed.
type ProviderState uint32

const (
	// ProviderStateDisable indicates the provider is being disabled.
	ProviderStateDisable ProviderState = iota
	// ProviderStateEnable indicates the provider is being enabled.
	ProviderStateEnable
	// ProviderStateCaptureState indicates the provider is having its current
	// state snap-shotted.
	ProviderStateCaptureState
)

type eventInfoClass uint32

const (
	eventInfoClassProviderBinaryTrackInfo eventInfoClass = iota
	eventInfoClassProviderSetReserved1
	eventInfoClassProviderSetTraits
	eventInfoClassProviderUseDescriptorType
)

// EnableCallback is the form of the callback function that receives provider
// enable/disable notifications from ETW.
type EnableCallback func(*guid.GUID, ProviderState, Level, uint64, uint64, uintptr)

func providerCallback(sourceID *guid.GUID, state ProviderState, level Level, matchAnyKeyword uint64, matchAllKeyword uint64, filterData uintptr, i uintptr) {
	provider := providers.getProvider(uint(i))

	switch state {
	case ProviderStateDisable:
		provider.enabled = false
	case ProviderStateEnable:
		provider.enabled = true
		provider.level = level
		provider.keywordAny = matchAnyKeyword
		provider.keywordAll = matchAllKeyword
	}

	if provider.callback != nil {
		provider.callback(sourceID, state, level, matchAnyKeyword, matchAllKeyword, filterData)
	}
}

// providerCallbackAdapter acts as the first-level callback from the C/ETW side
// for provider notifications. Because Go has trouble with callback arguments of
// different size, it has only pointer-sized arguments, which are then cast to
// the appropriate types when calling providerCallback.
func providerCallbackAdapter(sourceID *guid.GUID, state uintptr, level uintptr, matchAnyKeyword uintptr, matchAllKeyword uintptr, filterData uintptr, i uintptr) uintptr {
	providerCallback(sourceID, ProviderState(state), Level(level), uint64(matchAnyKeyword), uint64(matchAllKeyword), filterData, i)
	return 0
}

// providerIDFromName generates a provider ID based on the provider name. It
// uses the same algorithm as used by .NET's EventSource class, which is based
// on RFC 4122. More information on the algorithm can be found here:
// https://blogs.msdn.microsoft.com/dcook/2015/09/08/etw-provider-names-and-guids/
// The algorithm is roughly:
// Hash = Sha1(namespace + arg.ToUpper().ToUtf16be())
// Guid = Hash[0..15], with Hash[7] tweaked according to RFC 4122
func providerIDFromName(name string) *guid.GUID {
	buffer := sha1.New()

	namespace := []byte{0x48, 0x2C, 0x2D, 0xB2, 0xC3, 0x90, 0x47, 0xC8, 0x87, 0xF8, 0x1A, 0x15, 0xBF, 0xC1, 0x30, 0xFB}
	buffer.Write(namespace)

	binary.Write(buffer, binary.BigEndian, utf16.Encode([]rune(strings.ToUpper(name))))

	sum := buffer.Sum(nil)
	sum[7] = (sum[7] & 0xf) | 0x50

	return &guid.GUID{
		Data1: binary.LittleEndian.Uint32(sum[0:4]),
		Data2: binary.LittleEndian.Uint16(sum[4:6]),
		Data3: binary.LittleEndian.Uint16(sum[6:8]),
		Data4: [8]byte{sum[8], sum[9], sum[10], sum[11], sum[12], sum[13], sum[14], sum[15]},
	}
}

// NewProvider creates and registers a new ETW provider. The provider ID is
// generated based on the provider name.
func NewProvider(name string, callback EnableCallback) (provider *Provider, err error) {
	return NewProviderWithID(name, providerIDFromName(name), callback)
}

// Close unregisters the provider.
func (provider *Provider) Close() error {
	if provider == nil {
		return nil
	}

	providers.removeProvider(provider)
	return eventUnregister(provider.handle)
}

// IsEnabled calls IsEnabledForLevelAndKeywords with LevelAlways and all
// keywords set.
func (provider *Provider) IsEnabled() bool {
	return provider.IsEnabledForLevelAndKeywords(LevelAlways, ^uint64(0))
}

// IsEnabledForLevel calls IsEnabledForLevelAndKeywords with the specified level
// and all keywords set.
func (provider *Provider) IsEnabledForLevel(level Level) bool {
	return provider.IsEnabledForLevelAndKeywords(level, ^uint64(0))
}

// IsEnabledForLevelAndKeywords allows event producer code to check if there are
// any event sessions that are interested in an event, based on the event level
// and keywords. Although this check happens automatically in the ETW
// infrastructure, it can be useful to check if an event will actually be
// consumed before doing expensive work to build the event data.
func (provider *Provider) IsEnabledForLevelAndKeywords(level Level, keywords uint64) bool {
	if provider == nil {
		return false
	}

	if !provider.enabled {
		return false
	}

	// ETW automatically sets the level to 255 if it is specified as 0, so we
	// don't need to worry about the level=0 (all events) case.
	if level > provider.level {
		return false
	}

	if keywords != 0 && (keywords&provider.keywordAny == 0 || keywords&provider.keywordAll != provider.keywordAll) {
		return false
	}

	return true
}

// WriteEvent writes a single ETW event from the provider. The event is
// constructed based on the EventOpt and FieldOpt values that are passed as
// opts.
func (provider *Provider) WriteEvent(name string, eventOpts []EventOpt, fieldOpts []FieldOpt) error {
	if provider == nil {
		return nil
	}

	options := eventOptions{descriptor: newEventDescriptor()}
	em := &eventMetadata{}
	ed := &eventData{}

	// We need to evaluate the EventOpts first since they might change tags, and
	// we write out the tags before evaluating FieldOpts.
	for _, opt := range eventOpts {
		opt(&options)
	}

	if !provider.IsEnabledForLevelAndKeywords(options.descriptor.level, options.descriptor.keyword) {
		return nil
	}

	em.writeEventHeader(name, options.tags)

	for _, opt := range fieldOpts {
		opt(em, ed)
	}

	// Don't pass a data blob if there is no event data. There will always be
	// event metadata (e.g. for the name) so we don't need to do this check for
	// the metadata.
	dataBlobs := [][]byte{}
	if len(ed.bytes()) > 0 {
		dataBlobs = [][]byte{ed.bytes()}
	}

	return provider.writeEventRaw(options.descriptor, options.activityID, options.relatedActivityID, [][]byte{em.bytes()}, dataBlobs)
}

// writeEventRaw writes a single ETW event from the provider. This function is
// less abstracted than WriteEvent, and presents a fairly direct interface to
// the event writing functionality. It expects a series of event metadata and
// event data blobs to be passed in, which must conform to the TraceLogging
// schema. The functions on EventMetadata and EventData can help with creating
// these blobs. The blobs of each type are effectively concatenated together by
// the ETW infrastructure.
func (provider *Provider) writeEventRaw(
	descriptor *eventDescriptor,
	activityID *guid.GUID,
	relatedActivityID *guid.GUID,
	metadataBlobs [][]byte,
	dataBlobs [][]byte) error {

	dataDescriptorCount := uint32(1 + len(metadataBlobs) + len(dataBlobs))
	dataDescriptors := make([]eventDataDescriptor, 0, dataDescriptorCount)

	dataDescriptors = append(dataDescriptors, newEventDataDescriptor(eventDataDescriptorTypeProviderMetadata, provider.metadata))
	for _, blob := range metadataBlobs {
		dataDescriptors = append(dataDescriptors, newEventDataDescriptor(eventDataDescriptorTypeEventMetadata, blob))
	}
	for _, blob := range dataBlobs {
		dataDescriptors = append(dataDescriptors, newEventDataDescriptor(eventDataDescriptorTypeUserData, blob))
	}

	return eventWriteTransfer(provider.handle, descriptor, (*windows.GUID)(activityID), (*windows.GUID)(relatedActivityID), dataDescriptorCount, &dataDescriptors[0])
}
