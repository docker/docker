package config

import (
	"github.com/moby/moby/v2/daemon/config/extensions"
)

// ExtensionsConfig is a type alias for backward compatibility.
// Deprecated: Use extensions.Config instead. This type will be removed in a future release.
type ExtensionsConfig = extensions.Config

// ExtensionConfig is a type alias for backward compatibility.
// Deprecated: Use extensions.ExtensionConfig instead. This type will be removed in a future release.
type ExtensionConfig = extensions.ExtensionConfig

// ExtensionConfigs is a type alias for backward compatibility.
// Deprecated: Use extensions.ExtensionConfigs instead. This type will be removed in a future release.
type ExtensionConfigs = extensions.ExtensionConfigs
