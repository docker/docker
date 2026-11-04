package auxprogress

// OCIRegistryErrors contains raw errors as returned by the registry.
//
// https://github.com/opencontainers/distribution-spec/blob/main/spec.md#error-codes
type OCIRegistryErrors struct {
	Errors []OCIRegistryError `json:"errors"`
}

// OCIRegistryError is only used in OCIRegistryErrors - it's not a valid AUX
// message by itself.
type OCIRegistryError struct {
	Code    string      `json:"code"`
	Message string      `json:"message,omitempty"`
	Detail  interface{} `json:"detail,omitempty"`
}
