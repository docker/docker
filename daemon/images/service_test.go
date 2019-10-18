package images

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd"
	containers "github.com/containerd/containerd/api/services/containers/v1"
	diff "github.com/containerd/containerd/api/services/diff/v1"
	imagessrv "github.com/containerd/containerd/api/services/images/v1"
	namespacessrv "github.com/containerd/containerd/api/services/namespaces/v1"
	"github.com/containerd/containerd/content"
	_ "github.com/containerd/containerd/diff/walking/plugin"
	"github.com/containerd/containerd/events/exchange"
	_ "github.com/containerd/containerd/gc/scheduler"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	_ "github.com/containerd/containerd/services/containers"
	_ "github.com/containerd/containerd/services/content"
	_ "github.com/containerd/containerd/services/diff"
	_ "github.com/containerd/containerd/services/images"
	_ "github.com/containerd/containerd/services/leases"
	_ "github.com/containerd/containerd/services/namespaces"
	"github.com/containerd/containerd/services/server"
	srvconfig "github.com/containerd/containerd/services/server/config"
	_ "github.com/containerd/containerd/services/snapshots"
	"github.com/containerd/containerd/snapshots"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/archive"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	plugins    []*plugin.Registration
	pluginLoad sync.Once
)

func init() {
	logrus.SetLevel(logrus.DebugLevel)
	graphdriver.ApplyUncompressedLayer = archive.ApplyUncompressedLayer
}

func loadPlugins(ctx context.Context, config *srvconfig.Config) ([]*plugin.Registration, error) {
	var err error
	pluginLoad.Do(func() {
		plugins, err = server.LoadPlugins(ctx, config)
	})
	return plugins, err

}

func containerdServiceOpt(ctx context.Context, root string) (containerd.ClientOpt, error) {
	config := srvconfig.Config{
		Root:  filepath.Join(root, "root"),
		State: filepath.Join(root, "state"),
	}

	if err := os.MkdirAll(config.Root, 0711); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.State, 0711); err != nil {
		return nil, err
	}
	plugins, err := loadPlugins(ctx, &config)
	if err != nil {
		return nil, err
	}

	events := exchange.NewExchange()
	initialized := plugin.NewPluginSet()
	for _, p := range plugins {
		id := p.URI()
		log.G(ctx).WithField("type", p.Type).Infof("loading plugin %q...", id)

		initContext := plugin.NewContext(
			ctx,
			p,
			initialized,
			config.Root,
			config.State,
		)
		initContext.Events = events

		// load the plugin specific configuration if it is provided
		if p.Config != nil {
			pluginConfig, err := config.Decode(p.ID, p.Config)
			if err != nil {
				return nil, err
			}
			initContext.Config = pluginConfig
		}
		result := p.Init(initContext)
		if err := initialized.Add(result); err != nil {
			return nil, errors.Wrapf(err, "could not add plugin result to plugin set")
		}
	}

	initContext := plugin.NewContext(
		ctx,
		&plugin.Registration{
			Type: plugin.InternalPlugin,
			ID:   "unittest",
		},
		initialized,
		config.Root,
		config.State,
	)
	initContext.Events = events

	servicesOpts, err := getServicesOpts(initContext)
	if err != nil {
		return nil, err
	}

	return containerd.WithServices(servicesOpts...), nil
}

// getServicesOpts get service options from plugin context.
func getServicesOpts(ic *plugin.InitContext) ([]containerd.ServicesOpt, error) {
	plugins, err := ic.GetByType(plugin.ServicePlugin)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get service plugin")
	}

	opts := []containerd.ServicesOpt{
		containerd.WithEventService(ic.Events),
	}
	for s, fn := range map[string]func(interface{}) containerd.ServicesOpt{
		services.ContentService: func(s interface{}) containerd.ServicesOpt {
			return containerd.WithContentStore(s.(content.Store))
		},
		services.ImagesService: func(s interface{}) containerd.ServicesOpt {
			return containerd.WithImageService(s.(imagessrv.ImagesClient))
		},
		services.SnapshotsService: func(s interface{}) containerd.ServicesOpt {
			return containerd.WithSnapshotters(s.(map[string]snapshots.Snapshotter))
		},
		services.ContainersService: func(s interface{}) containerd.ServicesOpt {
			return containerd.WithContainerService(s.(containers.ContainersClient))
		},
		//services.TasksService: func(s interface{}) containerd.ServicesOpt {
		//	return containerd.WithTaskService(s.(tasks.TasksClient))
		//},
		services.DiffService: func(s interface{}) containerd.ServicesOpt {
			return containerd.WithDiffService(s.(diff.DiffClient))
		},
		services.NamespacesService: func(s interface{}) containerd.ServicesOpt {
			return containerd.WithNamespaceService(s.(namespacessrv.NamespacesClient))
		},
		services.LeasesService: func(s interface{}) containerd.ServicesOpt {
			return containerd.WithLeasesService(s.(leases.Manager))
		},
	} {
		p := plugins[s]
		if p == nil {
			return nil, errors.Errorf("service %q not found", s)
		}
		i, err := p.Instance()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get instance of service %q", s)
		}
		if i == nil {
			return nil, errors.Errorf("instance of service %q not found", s)
		}
		opts = append(opts, fn(i))
	}
	return opts, nil
}

type testFunc func(context.Context, *testing.T, *ImageService)

func setupTest(ctx context.Context, root string, service containerd.ClientOpt, fn testFunc) func(*testing.T) {
	return func(t *testing.T) {
		name := t.Name()
		name = name[strings.IndexByte(name, '/')+1:]
		root = filepath.Join(root, name)
		platform := platforms.DefaultSpec()

		ctx = namespaces.WithNamespace(ctx, name)

		client, err := containerd.New(
			"",
			containerd.WithDefaultNamespace(name),
			service,
		)
		if err != nil {
			t.Fatalf("Failed to get containerd client: %v", err)
		}

		// TODO(containerd): Use a mocked layer store or one backed by containerd?
		idMapping, err := getIDMapping()
		if err != nil {
			t.Fatal(err)
		}
		ls, err := layer.NewStoreFromOptions(layer.StoreOptions{
			Root:                      root,
			GraphDriver:               testgraphdriver,
			MetadataStorePathTemplate: filepath.Join(root, "layerdb"),
			IDMapping:                 idMapping,
			OS:                        platform.OS,
		})
		if err != nil {
			t.Fatalf("Failed to initialize layer store: %v", err)
		}

		config := ImageServiceConfig{
			DefaultNamespace: name,
			DefaultPlatform:  platforms.DefaultSpec(),
			Client:           client,
			LayerBackends: []LayerBackend{
				{
					Store:    ls,
					Platform: platforms.Only(platform),
				},
			},
			//ContainerStore         containerStore
			//EventsService          *daemonevents.Events
			//MaxConcurrentDownloads: 3,
			//MaxConcurrentUploads:   3,
		}

		fn(ctx, t, NewImageService(config))
	}
}

func TestImageService(t *testing.T) {
	ctx := context.Background()
	td, err := ioutil.TempDir("", "imagetest-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(td); err != nil {
			t.Errorf("Failed to remove temp dir %s: %s", td, err)
		}
	}()
	service, err := containerdServiceOpt(ctx, td)
	if err != nil {
		t.Error(err)
		return
	}

	t.Run("ListImages", setupTest(ctx, td, service, testListImages))
	t.Run("DeleteImages", setupTest(ctx, td, service, testDeleteImages))
}
