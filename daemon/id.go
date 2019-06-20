package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/libtrust"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

func loadOrCreateID(path, deprecatedTrustKeyPath string) (string, error) {
	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return "", err
	}
	var id string
	idb, err := ioutil.ReadFile(path)
	if os.IsNotExist(err) {
		// first try to fallback to trust-key based ID
		trustKey, err := libtrust.LoadKeyFile(deprecatedTrustKeyPath)
		if err == nil {
			id = trustKey.PublicKey().KeyID()
		} else {
			if err != libtrust.ErrKeyFileDoesNotExist {
				logrus.Warnf("Error loading deprecated key file %s (%v). Falling back to generating new ID instead", deprecatedTrustKeyPath, err)
			}
			// then fallback to generating UUID
			id = uuid.New().String()
		}
		if err := ioutils.AtomicWriteFile(path, []byte(id), os.FileMode(0600)); err != nil {
			return "", fmt.Errorf("Error saving ID file: %s", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("Error loading ID file %s: %s", path, err)
	} else {
		id = string(idb)
	}
	return id, nil
}
