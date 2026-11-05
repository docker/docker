package plugins

import (
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/sys/userns"
	"github.com/pkg/errors"
)

// ErrNotFound plugin not found
var ErrNotFound = errors.New("plugin not found")

const defaultSocketsPath = "/run/docker/plugins"

// LocalRegistry defines a registry that is local (using unix socket).
type LocalRegistry struct {
	socketsPath string
	specsPaths  []string
}

func NewLocalRegistry() LocalRegistry {
	return LocalRegistry{
		socketsPath: defaultSocketsPath,
		specsPaths:  specsPaths(),
	}
}

// Scan scans all the plugin paths and returns all the names it found
func (l *LocalRegistry) Scan() ([]string, error) {
	var names []string
	dirEntries, err := os.ReadDir(l.socketsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, errors.Wrap(err, "error reading dir entries")
	}

	for _, entry := range dirEntries {
		if entry.IsDir() {
			fi, err := os.Stat(filepath.Join(l.socketsPath, entry.Name(), entry.Name()+".sock"))
			if err != nil {
				continue
			}

			entry = fs.FileInfoToDirEntry(fi)
		}

		if entry.Type()&os.ModeSocket != 0 {
			names = append(names, strings.TrimSuffix(filepath.Base(entry.Name()), filepath.Ext(entry.Name())))
		}
	}

	for _, p := range l.specsPaths {
		dirEntries, err = os.ReadDir(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if os.IsPermission(err) && userns.RunningInUserNS() {
				log.L.Debug(err.Error())
				continue
			}
			return nil, errors.Wrap(err, "error reading dir entries")
		}
		for _, entry := range dirEntries {
			if entry.IsDir() {
				infos, err := os.ReadDir(filepath.Join(p, entry.Name()))
				if err != nil {
					continue
				}

				for _, info := range infos {
					if strings.TrimSuffix(info.Name(), filepath.Ext(info.Name())) == entry.Name() {
						entry = info
						break
					}
				}
			}

			switch ext := filepath.Ext(entry.Name()); ext {
			case ".spec", ".json":
				plugin := strings.TrimSuffix(entry.Name(), ext)
				names = append(names, plugin)
			default:
			}
		}
	}
	return names, nil
}

// Plugin returns the plugin registered with the given name (or returns an error).
func (l *LocalRegistry) Plugin(name string) (*Plugin, error) {
	// name is used to build filesystem paths for the plugin's socket and spec
	// files (see pluginPaths). Reject names that are not a single, local path
	// element so that a crafted name such as "../other" cannot escape the
	// configured plugin directories and load an out-of-scope spec or socket.
	if !validName(name) {
		return nil, errors.Wrapf(ErrNotFound, "invalid v1 plugin name %q", name)
	}

	socketPaths := pluginPaths(l.socketsPath, name, ".sock")
	for _, p := range socketPaths {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return NewLocalPlugin(name, "unix://"+p), nil
		}
	}

	var txtSpecPaths []string
	for _, p := range l.specsPaths {
		txtSpecPaths = append(txtSpecPaths, pluginPaths(p, name, ".spec")...)
		txtSpecPaths = append(txtSpecPaths, pluginPaths(p, name, ".json")...)
	}

	for _, p := range txtSpecPaths {
		if _, err := os.Stat(p); err == nil {
			if strings.HasSuffix(p, ".json") {
				return readPluginJSONInfo(name, p)
			}
			return readPluginInfo(name, p)
		}
	}
	return nil, errors.Wrapf(ErrNotFound, "could not find plugin %s in v1 plugin registry", name)
}

// SpecsPaths returns paths in which to look for plugins, in order of priority.
//
// On Windows:
//
//   - "%programdata%\docker\plugins"
//
// On Unix in non-rootless mode:
//
//   - "/etc/docker/plugins"
//   - "/usr/lib/docker/plugins"
//
// On Unix in rootless-mode:
//
//   - "$XDG_CONFIG_HOME/docker/plugins" (or "/etc/docker/plugins" if $XDG_CONFIG_HOME is not set)
//   - "$HOME/.local/lib/docker/plugins" (pr "/usr/lib/docker/plugins" if $HOME is set)
func SpecsPaths() []string {
	return specsPaths()
}

func readPluginInfo(name, path string) (*Plugin, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	addr := strings.TrimSpace(string(content))

	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}

	if u.Scheme == "" {
		return nil, errors.New("Unknown protocol")
	}

	return NewLocalPlugin(name, addr), nil
}

func readPluginJSONInfo(name, path string) (*Plugin, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var p Plugin
	if err := json.NewDecoder(f).Decode(&p); err != nil {
		return nil, err
	}
	p.name = name
	if p.TLSConfig != nil && p.TLSConfig.CAFile == "" {
		p.TLSConfig.InsecureSkipVerify = true
	}
	p.activateWait = sync.NewCond(&sync.Mutex{})

	return &p, nil
}

func pluginPaths(base, name, ext string) []string {
	return []string{
		filepath.Join(base, name+ext),
		filepath.Join(base, name, name+ext),
	}
}

// validName reports whether name is safe to use as a single path element when
// locating a plugin's socket and spec files. It rejects the "." and ".."
// directory entries and any non-local name (empty, absolute, containing a path
// separator, or, on Windows, a reserved device name), all of which could
// otherwise be joined with a base directory to reference files outside it.
func validName(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	return filepath.IsLocal(name)
}
