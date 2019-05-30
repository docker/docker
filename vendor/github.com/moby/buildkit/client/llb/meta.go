package llb

import (
	"fmt"
	"net"
	"path"

	"github.com/containerd/containerd/platforms"
	"github.com/google/shlex"
	"github.com/moby/buildkit/solver/pb"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

type contextKeyT string

var (
	keyArgs      = contextKeyT("llb.exec.args")
	keyDir       = contextKeyT("llb.exec.dir")
	keyEnv       = contextKeyT("llb.exec.env")
	keyUser      = contextKeyT("llb.exec.user")
	keyExtraHost = contextKeyT("llb.exec.extrahost")
	keyPlatform  = contextKeyT("llb.platform")
	keyNetwork   = contextKeyT("llb.network")
	keySecurity  = contextKeyT("llb.security")
)

func addEnvf(key, value string, v ...interface{}) StateOption {
	return func(s State) State {
		return s.WithValue(keyEnv, getEnv(s).AddOrReplace(key, fmt.Sprintf(value, v...)))
	}
}

func dir(str string) StateOption {
	return dirf(str)
}

func dirf(str string, v ...interface{}) StateOption {
	return func(s State) State {
		value := fmt.Sprintf(str, v...)
		if !path.IsAbs(value) {
			prev := getDir(s)
			if prev == "" {
				prev = "/"
			}
			value = path.Join(prev, value)
		}
		return s.WithValue(keyDir, value)
	}
}

func user(str string) StateOption {
	return func(s State) State {
		return s.WithValue(keyUser, str)
	}
}

func reset(s_ State) StateOption {
	return func(s State) State {
		s = NewState(s.Output())
		s.ctx = s_.ctx
		return s
	}
}

func getEnv(s State) EnvList {
	v := s.Value(keyEnv)
	if v != nil {
		return v.(EnvList)
	}
	return EnvList{}
}

func getDir(s State) string {
	v := s.Value(keyDir)
	if v != nil {
		return v.(string)
	}
	return ""
}

func getArgs(s State) []string {
	v := s.Value(keyArgs)
	if v != nil {
		return v.([]string)
	}
	return nil
}

func getUser(s State) string {
	v := s.Value(keyUser)
	if v != nil {
		return v.(string)
	}
	return ""
}

func args(args ...string) StateOption {
	return func(s State) State {
		return s.WithValue(keyArgs, args)
	}
}

func shlexf(str string, v ...interface{}) StateOption {
	return func(s State) State {
		arg, err := shlex.Split(fmt.Sprintf(str, v...))
		if err != nil {
			// TODO: handle error
		}
		return args(arg...)(s)
	}
}

func platform(p specs.Platform) StateOption {
	return func(s State) State {
		return s.WithValue(keyPlatform, platforms.Normalize(p))
	}
}

func getPlatform(s State) *specs.Platform {
	v := s.Value(keyPlatform)
	if v != nil {
		p := v.(specs.Platform)
		return &p
	}
	return nil
}

func extraHost(host string, ip net.IP) StateOption {
	return func(s State) State {
		return s.WithValue(keyExtraHost, append(getExtraHosts(s), HostIP{Host: host, IP: ip}))
	}
}

func getExtraHosts(s State) []HostIP {
	v := s.Value(keyExtraHost)
	if v != nil {
		return v.([]HostIP)
	}
	return nil
}

type HostIP struct {
	Host string
	IP   net.IP
}

func network(v pb.NetMode) StateOption {
	return func(s State) State {
		return s.WithValue(keyNetwork, v)
	}
}
func getNetwork(s State) pb.NetMode {
	v := s.Value(keyNetwork)
	if v != nil {
		n := v.(pb.NetMode)
		return n
	}
	return NetModeSandbox
}

func security(v pb.SecurityMode) StateOption {
	return func(s State) State {
		return s.WithValue(keySecurity, v)
	}
}
func getSecurity(s State) pb.SecurityMode {
	v := s.Value(keySecurity)
	if v != nil {
		n := v.(pb.SecurityMode)
		return n
	}
	return SecurityModeSandbox
}

type EnvList []KeyValue

type KeyValue struct {
	key   string
	value string
}

func (e EnvList) AddOrReplace(k, v string) EnvList {
	e = e.Delete(k)
	e = append(e, KeyValue{key: k, value: v})
	return e
}

func (e EnvList) SetDefault(k, v string) EnvList {
	if _, ok := e.Get(k); !ok {
		e = append(e, KeyValue{key: k, value: v})
	}
	return e
}

func (e EnvList) Delete(k string) EnvList {
	e = append([]KeyValue(nil), e...)
	if i, ok := e.Index(k); ok {
		return append(e[:i], e[i+1:]...)
	}
	return e
}

func (e EnvList) Get(k string) (string, bool) {
	if index, ok := e.Index(k); ok {
		return e[index].value, true
	}
	return "", false
}

func (e EnvList) Index(k string) (int, bool) {
	for i, kv := range e {
		if kv.key == k {
			return i, true
		}
	}
	return -1, false
}

func (e EnvList) ToArray() []string {
	out := make([]string, 0, len(e))
	for _, kv := range e {
		out = append(out, kv.key+"="+kv.value)
	}
	return out
}
