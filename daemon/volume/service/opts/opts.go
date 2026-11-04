package opts

// CreateOption is used to pass options in when creating a volume
type CreateOption func(*CreateConfig)

// CreateConfig is the set of config options that can be set when creating
// a volume
type CreateConfig struct {
	Options   map[string]string
	Labels    map[string]string
	Reference string
}

// WithCreateLabel creates a CreateOption which adds a label with the given key/value pair
func WithCreateLabel(key, value string) CreateOption {
	return func(cfg *CreateConfig) {
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		cfg.Labels[key] = value
	}
}

// WithCreateLabels creates a CreateOption which sets the labels to the
// passed in value
func WithCreateLabels(labels map[string]string) CreateOption {
	return func(cfg *CreateConfig) {
		cfg.Labels = labels
	}
}

// WithCreateOptions creates a CreateOption which sets the options passed
// to the volume driver when creating a volume to the options passed in.
func WithCreateOptions(opts map[string]string) CreateOption {
	return func(cfg *CreateConfig) {
		cfg.Options = opts
	}
}

// WithCreateReference creates a CreateOption which sets a reference to use
// when creating a volume. This ensures that the volume is created with a reference
// already attached to it to prevent race conditions with Create and volume cleanup.
func WithCreateReference(ref string) CreateOption {
	return func(cfg *CreateConfig) {
		cfg.Reference = ref
	}
}

// GetConfig is used with `GetOption` to set options for the volumes service's
// `Get` implementation.
type GetConfig struct {
	Driver        string
	Reference     string
	ResolveStatus bool
	ResolveSize   bool
}

// GetOption is passed to the service `Get` add extra details on the get request
type GetOption func(*GetConfig)

// WithGetDriver provides the driver to get the volume from
// If no driver is provided to `Get`, first the available metadata is checked
// to see which driver it belongs to, if that is not available all drivers are
// probed to find the volume.
func WithGetDriver(name string) GetOption {
	return func(o *GetConfig) {
		o.Driver = name
	}
}

// WithGetReference indicates to `Get` to increment the reference count for the
// retrieved volume with the provided reference ID.
func WithGetReference(ref string) GetOption {
	return func(o *GetConfig) {
		o.Reference = ref
	}
}

// WithGetResolveStatus indicates to `Get` to also fetch the volume status.
// This can cause significant overhead in the volume lookup.
func WithGetResolveStatus(cfg *GetConfig) {
	cfg.ResolveStatus = true
}

// WithGetResolveSize indicates to `Get` to also calculate the volume size.
// This can cause significant overhead in the volume lookup.
func WithGetResolveSize(cfg *GetConfig) {
	cfg.ResolveSize = true
}

// RemoveConfig is used by `RemoveOption` to store config options for remove
type RemoveConfig struct {
	PurgeOnError bool
}

// RemoveOption is used to pass options to the volumes service `Remove` implementation
type RemoveOption func(*RemoveConfig)

// WithPurgeOnError is an option passed to `Remove` which will purge all cached
// data about a volume even if there was an error while attempting to remove the
// volume.
func WithPurgeOnError(b bool) RemoveOption {
	return func(o *RemoveConfig) {
		o.PurgeOnError = b
	}
}
