package portallocator

const (
	StartPortRange = 60000
	EndPortRange   = 65000
)

func getDynamicPortRange() (start int, end int, err error) {
	return StartPortRange, EndPortRange, nil
}
