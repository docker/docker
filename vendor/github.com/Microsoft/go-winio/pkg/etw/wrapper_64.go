// +build amd64 arm64

package etw

import (
	"golang.org/x/sys/windows"
)

func eventUnregister(providerHandle providerHandle) (win32err error) {
	return eventUnregister_64(providerHandle)
}

func eventWriteTransfer(
	providerHandle providerHandle,
	descriptor *eventDescriptor,
	activityID *windows.GUID,
	relatedActivityID *windows.GUID,
	dataDescriptorCount uint32,
	dataDescriptors *eventDataDescriptor) (win32err error) {

	return eventWriteTransfer_64(
		providerHandle,
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

	return eventSetInformation_64(
		providerHandle,
		class,
		information,
		length)
}
