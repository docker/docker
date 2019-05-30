// +build !arm

package binfmt_misc

func armSupported() error {
	return check(Binaryarm)
}
