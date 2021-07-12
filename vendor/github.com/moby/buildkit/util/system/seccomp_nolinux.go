// +build !linux

package system

func SeccompSupported() bool {
	return false
}
