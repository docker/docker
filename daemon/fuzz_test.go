//go:build linux

package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	fuzz "github.com/AdaLogics/go-fuzz-headers"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/image"
	dockerreference "github.com/docker/docker/reference"
	"github.com/moby/sys/mount"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

var (
	initter sync.Once
	funcMap = map[int]string{
		0:  "*Daemon.imageService.GetImage",
		1:  "*Daemon.GetContainer",
		2:  "*Daemon.containers.Add",
		3:  "*Daemon.containers.Delete",
		4:  "*Daemon.containers.List",
		5:  "*Daemon.containers.Size",
		6:  "*Daemon.ContainerCopy",
		7:  "*Daemon.ContainerAttach",
		8:  "*Daemon.imageService.CreateImage",
		9:  "*Daemon.ContainerCreate",
		10: "*Daemon.ContainerChanges",
	}
)

func IsJSON(input []byte) bool {
	var js json.RawMessage
	return json.Unmarshal(input, &js) == nil
}

func FuzzDaemon(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		inittier.Do(func() {
			logrus.SetLevel(logrus.ErrorLevel)
		})
		if os.Getuid() != 0 {
			t.Skipf("skipping test that requires root")
		}

		f := fuzz.NewConsumer(data)

		// reference store
		referenceStoreJson, err := f.GetBytes()
		if err != nil {
			t.Skip()
		}

		if !IsJSON(referenceStoreJson) {
			t.Skip()
		}
		rsDir, err := os.MkdirTemp("", "referenceStore")
		if err != nil {
			t.Skip()
		}
		defer os.RemoveAll(rsDir)
		jsonFile := filepath.Join(rsDir, "repositories.json")
		err = os.WriteFile(jsonFile, referenceStoreJson, 0o666)
		if err != nil {
			panic(err)
		}
		rStore, err := dockerreference.NewReferenceStore(jsonFile)
		if err != nil {
			t.Skip()
		}
		// end reference store

		testRoot, err := os.MkdirTemp("", "test-dir")
		if err != nil {
			t.Skip()
		}
		defer os.RemoveAll(testRoot)

		err = mount.MakePrivate(testRoot)
		if err != nil {
			t.Skip()
		}

		cfg := &config.Config{}
		defer mount.Unmount(testRoot)

		cfg.ExecRoot = filepath.Join(testRoot, "exec")
		cfg.Root = filepath.Join(testRoot, "daemon")

		err = os.Mkdir(cfg.ExecRoot, 0755)
		if err != nil {
			t.Skip()
		}
		err = os.Mkdir(cfg.Root, 0755)
		if err != nil {
			t.Skip()
		}

		imgStoreDir, err := os.MkdirTemp("", "images-fs-store")
		if err != nil {
			panic(err)
		}

		fsBackend, err := image.NewFSStoreBackend(imgStoreDir)
		if err != nil {
			panic(err)
		}

		defer os.RemoveAll(imgStoreDir)

		is, err := image.NewImageStore(fsBackend, nil)
		if err != nil {
			panic(err)
		}

		d := &Daemon{
			root:         cfg.Root,
			imageService: images.NewImageService(images.ImageServiceConfig{ReferenceStore: rStore, ImageStore: is}),
			containers:   container.NewMemoryStore(),
		}

		configStore := &configStore{Config: *cfg}
		d.configStore.Store(configStore)

		containersReplica, err := container.NewViewDB()
		if err != nil {
			panic(err)
		}

		d.containersReplica = containersReplica

		var noOfCalls int
		noOfCalls, err = f.GetInt()
		if err != nil {
			t.Skip()
		}
		if noOfCalls < 2 {
			noOfCalls = 2
		}
		for i := 0; i < noOfCalls%10; i++ {
			typeOfCall, err := f.GetInt()
			if err != nil {
				t.Skip()
			}
			switch funcMap[typeOfCall%11] {
			case "*Daemon.imageService.GetImage":
				err := getImage(d, f)
				if err != nil {
					t.Skip()
				}
			case "*Daemon.GetContainer":
				err := getContainer(d, f)
				if err != nil {
					t.Skip()
				}
			case "*Daemon.containers.Add":
				addContainer(d, f)
			case "*Daemon.containers.Delete":
				refOrID, err := f.GetString()
				if err != nil {
					t.Skip()
				}
				d.containers.Delete(refOrID)
			case "*Daemon.containers.List":
				d.containers.List()
			case "*Daemon.containers.Size":
				d.containers.Size()
			case "*Daemon.ContainerCopy":
				err := containerCopy(d, f)
				if err != nil {
					t.Skip()
				}
			case "*Daemon.ContainerAttach":
				err := containerAttach(d, f)
				if err != nil {
					t.Skip()
				}
			case "*Daemon.imageService.CreateImage":
				err := createImage(d, f)
				if err != nil {
					t.Skip()
				}
			case "*Daemon.ContainerCreate":
				containerCreate(d, f)
			case "*Daemon.ContainerChanges":
				name, err := f.GetString()
				if err != nil {
					t.Skip()
				}
				_, _ = d.ContainerChanges(context.Background(), name)
			}
		}
	})
}

func addContainer(d *Daemon, f *fuzz.ConsumeFuzzer) {
	c := &container.Container{}
	f.GenerateStruct(c)
	if c.ID == "" {
		return
	}
	c.State = container.NewState()
	c.ExecCommands = container.NewExecStore()
	d.containers.Add(c.ID, c)
}

func getImage(d *Daemon, f *fuzz.ConsumeFuzzer) error {
	var refOrID string
	refOrID, err := f.GetString()
	if err != nil {
		return err
	}
	if refOrID == "" {
		refOrID = "ref"
	}
	options := imagetypes.GetImageOpts{}
	f.GenerateStruct(&options)
	_, _ = d.imageService.GetImage(context.Background(), refOrID, options)
	return nil
}

func getContainer(d *Daemon, f *fuzz.ConsumeFuzzer) error {
	getContainer, err := f.GetString()
	if err != nil {
		return err
	}
	_, _ = d.GetContainer(getContainer)
	return nil
}

func containerCopy(d *Daemon, f *fuzz.ConsumeFuzzer) error {
	name, err := f.GetString()
	if err != nil {
		return err
	}
	res, err := f.GetString()
	if err != nil {
		return err
	}
	_, _ = d.ContainerCopy(name, res)
	return nil
}

func containerAttach(d *Daemon, f *fuzz.ConsumeFuzzer) error {
	prefixOrName, err := f.GetString()
	if err != nil {
		return err
	}
	c := &backend.ContainerAttachConfig{}
	f.GenerateStruct(c)
	d.ContainerAttach(prefixOrName, c)
	return nil
}

func createImage(d *Daemon, f *fuzz.ConsumeFuzzer) error {
	imageConfig, err := f.GetBytes()
	if err != nil {
		return err
	}
	parent, err := f.GetString()
	if err != nil {
		return err
	}
	dig := digest.FromBytes([]byte("fuzz"))
	_, _ = d.imageService.CreateImage(context.Background(), imageConfig, parent, dig)
	return nil
}

func containerCreate(d *Daemon, f *fuzz.ConsumeFuzzer) error {
	params := &types.ContainerCreateConfig{}
	f.GenerateStruct(params)
	_, _ = d.ContainerCreate(context.Background(), *params)
	return nil
}
