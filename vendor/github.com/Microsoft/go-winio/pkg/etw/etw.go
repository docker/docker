// Package etw provides support for TraceLogging-based ETW (Event Tracing
// for Windows). TraceLogging is a format of ETW events that are self-describing
// (the event contains information on its own schema). This allows them to be
// decoded without needing a separate manifest with event information. The
// implementation here is based on the information found in
// TraceLoggingProvider.h in the Windows SDK, which implements TraceLogging as a
// set of C macros.
package etw

//go:generate go run mksyscall_windows.go -output zsyscall_windows.go etw.go

//sys eventRegister(providerId *windows.GUID, callback uintptr, callbackContext uintptr, providerHandle *providerHandle) (win32err error) = advapi32.EventRegister

//sys eventUnregister_64(providerHandle providerHandle) (win32err error) = advapi32.EventUnregister
//sys eventWriteTransfer_64(providerHandle providerHandle, descriptor *eventDescriptor, activityID *windows.GUID, relatedActivityID *windows.GUID, dataDescriptorCount uint32, dataDescriptors *eventDataDescriptor) (win32err error) = advapi32.EventWriteTransfer
//sys eventSetInformation_64(providerHandle providerHandle, class eventInfoClass, information uintptr, length uint32) (win32err error) = advapi32.EventSetInformation

//sys eventUnregister_32(providerHandle_low uint32, providerHandle_high uint32) (win32err error) = advapi32.EventUnregister
//sys eventWriteTransfer_32(providerHandle_low uint32, providerHandle_high uint32, descriptor *eventDescriptor, activityID *windows.GUID, relatedActivityID *windows.GUID, dataDescriptorCount uint32, dataDescriptors *eventDataDescriptor) (win32err error) = advapi32.EventWriteTransfer
//sys eventSetInformation_32(providerHandle_low uint32, providerHandle_high uint32, class eventInfoClass, information uintptr, length uint32) (win32err error) = advapi32.EventSetInformation
