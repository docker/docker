// +build 386 arm

package etw

import (
	"golang.org/x/sys/windows"
)

func low(v providerHandle) uint32 {
	return uint32(v & 0xffffffff)
}

func high(v providerHandle) uint32 {
	return low(v >> 32)
}

func eventUnregister(providerHandle providerHandle) (win32err error) {
	return eventUnregister_32(low(providerHandle), high(providerHandle))
}

func eventWriteTransfer(
	providerHandle providerHandle,
	descriptor *eventDescriptor,
	activityID *windows.GUID,
	relatedActivityID *windows.GUID,
	dataDescriptorCount uint32,
	dataDescriptors *eventDataDescriptor) (win32err error) {

	return eventWriteTransfer_32(
		low(providerHandle),
		high(providerHandle),
		descriptor,
		activityID,
		relatedActivityID,
		dataDescriptorCount,
		dataDescriptors)
}

func eventSetInformation(
	providerHandle providerHandle,
	class eventInfoClass,
	information uintptr,
	length uint32) (win32err error) {

	return eventSetInformation_32(
		low(providerHandle),
		high(providerHandle),
		class,
		information,
		length)
}
