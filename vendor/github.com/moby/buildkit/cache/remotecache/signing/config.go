package signing

import "github.com/sigstore/sigstore-go/pkg/fulcio/certificate"

// Config controls signing and verification of remote cache metadata.
type Config struct {
	Sign   *SignConfig  `toml:"sign"`
	Verify VerifyConfig `toml:"verify"`
}

// SignConfig specifies the command that reads a payload and writes its bundle.
type SignConfig struct {
	Command []string `toml:"command"`
}

// VerifyConfig controls whether imported cache metadata must be verified.
type VerifyConfig struct {
	Required bool         `toml:"required"`
	Policy   VerifyPolicy `toml:"policy"`
}

// VerifyPolicy constrains the verified timestamps and signing certificate.
type VerifyPolicy struct {
	TimestampThreshold int  `toml:"timestampThreshold"`
	TlogThreshold      *int `toml:"tlogThreshold"`
	certificate.Summary
}
