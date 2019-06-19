package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/docker/docker/pkg/ioutils"
	"github.com/google/uuid"
)

func loadOrCreateUUID(path string) (string, error) {
	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return "", err
	}
	var id string
	idb, err := ioutil.ReadFile(path)
	if os.IsNotExist(err) {
		id = uuid.New().String()
		if err := ioutils.AtomicWriteFile(path, []byte(id), os.FileMode(0600)); err != nil {
			return "", fmt.Errorf("Error saving uuid file: %s", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("Error loading uuid file %s: %s", path, err)
	} else {
		idp, err := uuid.Parse(string(idb))
		if err != nil {
			return "", fmt.Errorf("Error parsing uuid in file %s: %s", path, err)
		}
		id = idp.String()
	}
	return id, nil
}
