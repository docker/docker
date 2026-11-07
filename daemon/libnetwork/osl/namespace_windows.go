package osl

type Namespace struct{}

func (n *Namespace) Destroy() error { return nil }

// NewSandbox provides a new sandbox instance created in an os specific way
// provided a key which uniquely identifies the sandbox
func NewSandbox(key string, osCreate, isRestore bool) (*Namespace, error) {
	return nil, nil
}

func GetSandboxForExternalKey(path string, key string) (*Namespace, error) {
	return nil, nil
}
