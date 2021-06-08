module github.com/docker/docker

go 1.13

require (
	cloud.google.com/go v0.81.0
	cloud.google.com/go/logging v1.4.2
	code.cloudfoundry.org/clock v1.0.0 // indirect
	github.com/Graylog2/go-gelf v0.0.0-20191017102106-1550ee647df0
	github.com/Microsoft/go-winio v0.4.19
	github.com/Microsoft/hcsshim v0.8.16
	github.com/Microsoft/opengcs v0.3.10-0.20190304234800-a10967154e14
	github.com/RackSec/srslog v0.0.0-20180709174129-a4725f04ec91
	github.com/armon/go-radix v0.0.0-20180808171621-7fddfc383310
	github.com/aws/aws-sdk-go v1.31.6
	github.com/bsphere/le_go v0.0.0-20170215134836-7a984a84b549
	github.com/cloudflare/cfssl v0.0.0-20180323000720-5d63dbd981b5
	github.com/containerd/cgroups v1.0.1
	github.com/containerd/containerd v1.5.2
	github.com/containerd/continuity v0.1.0
	github.com/containerd/fifo v1.0.0
	github.com/containerd/typeurl v1.0.2
	github.com/coreos/etcd v3.3.25+incompatible // indirect
	github.com/coreos/go-systemd/v22 v22.3.1
	github.com/creack/pty v1.1.11
	github.com/deckarep/golang-set v0.0.0-20141123011944-ef32fa3046d9
	github.com/docker/distribution v2.7.1+incompatible
	github.com/docker/go-connections v0.4.0
	github.com/docker/go-events v0.0.0-20190806004212-e31b211e4f1c
	github.com/docker/go-metrics v0.0.1
	github.com/docker/go-units v0.4.0
	github.com/docker/libkv v0.2.2-0.20180912205406-458977154600
	github.com/docker/libtrust v0.0.0-20150526203908-9cbd2a1374f4
	github.com/docker/swarmkit v0.0.0-20210427195336-60d87cb7cdb0
	github.com/fernet/fernet-go v0.0.0-20180830025343-9eac43b88a5e // indirect
	github.com/fluent/fluent-logger-golang v1.6.1
	github.com/fsnotify/fsnotify v1.4.9
	github.com/godbus/dbus/v5 v5.0.4
	github.com/gogo/protobuf v1.3.2
	github.com/golang/gddo v0.0.0-20190904175337-72a348e765d2
	github.com/google/certificate-transparency-go v1.0.20 // indirect
	github.com/google/go-cmp v0.5.5
	github.com/google/uuid v1.2.0
	github.com/gorilla/mux v1.8.0
	github.com/hashicorp/go-immutable-radix v1.0.0
	github.com/hashicorp/go-memdb v0.0.0-20161216180745-cb9a474f84cc
	github.com/hashicorp/go-sockaddr v1.0.2 // indirect
	github.com/hashicorp/memberlist v0.1.3
	github.com/hashicorp/serf v0.8.2
	github.com/imdario/mergo v0.3.11
	github.com/ishidawataru/sctp v0.0.0-20210226210310-f2269e66cdee
	github.com/miekg/dns v1.1.27
	github.com/mistifyio/go-zfs v2.1.2-0.20190413222219-f784269be439+incompatible
	github.com/moby/buildkit v0.6.2-0.20210601220845-7e03277b32d4
	github.com/moby/ipvs v1.0.1
	github.com/moby/locker v1.0.1
	github.com/moby/sys/mount v0.1.1
	github.com/moby/sys/mountinfo v0.4.1
	github.com/moby/sys/symlink v0.1.0
	github.com/moby/term v0.0.0-20201110203204-bea5bbe245bf
	github.com/morikuni/aec v1.0.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.1
	github.com/opencontainers/runc v1.0.0-rc95
	github.com/opencontainers/runtime-spec v1.0.3-0.20210326190908-1c3f411f0417
	github.com/opencontainers/selinux v1.8.0
	github.com/pelletier/go-toml v1.8.1
	github.com/phayes/permbits v0.0.0-20190612203442-39d7c581d2ee // indirect
	github.com/philhofer/fwd v1.0.0 // indirect
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.7.1
	github.com/samuel/go-zookeeper v0.0.0-20150415181332-d0e0d8e11f31 // indirect
	github.com/sirupsen/logrus v1.7.0
	github.com/spf13/cobra v1.1.3
	github.com/spf13/pflag v1.0.5
	github.com/syndtr/gocapability v0.0.0-20200815063812-42c35b437635
	github.com/tchap/go-patricia v2.3.0+incompatible
	github.com/tedsuo/ifrit v0.0.0-20191009134036-9a97d0632f00 // indirect
	github.com/tinylib/msgp v1.1.0 // indirect
	github.com/tonistiigi/fsutil v0.0.0-20201103201449-0834f99b7b85
	github.com/urfave/cli v1.22.2
	github.com/vbatts/tar-split v0.11.1
	github.com/vishvananda/netlink v1.1.1-0.20201029203352-d40f9887b852
	github.com/vishvananda/netns v0.0.0-20200728191858-db3c7e526aae
	go.etcd.io/bbolt v1.3.5
	golang.org/x/net v0.0.0-20210503060351-7fd8e65b6420
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/sys v0.0.0-20210503080704-8803ae5d1324
	golang.org/x/time v0.0.0-20200630173020-3af7569d3a1e
	google.golang.org/genproto v0.0.0-20210517163617-5e0236093d7a
	google.golang.org/grpc v1.37.1
	google.golang.org/grpc/examples v0.0.0-20210608045717-7301a311748c // indirect
	gotest.tools/v3 v3.0.3
)

replace github.com/moby/buildkit => github.com/cpuguy83/buildkit v0.6.2-0.20210601220845-7e03277b32d4

replace github.com/docker/swarmkit => github.com/cpuguy83/swarmkit v0.0.0-20210427195336-60d87cb7cdb0

replace github.com/hashicorp/go-immutable-radix => github.com/tonistiigi/go-immutable-radix v0.0.0-20170803185627-826af9ccf0fe
