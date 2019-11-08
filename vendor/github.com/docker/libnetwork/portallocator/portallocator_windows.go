package portallocator

func init() {
	defaultPortRangeStart = 60000
	defaultPortRangeEnd = 65000
}

func getDynamicPortRange() (start int, end int, err error) {
	return defaultPortRangeStart, defaultPortRangeEnd, nil
}
