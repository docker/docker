// 'vendor.mod' enables use of 'go mod vendor' to managed 'vendor/' directory.
// There is no 'go.mod' file, as that would imply opting in for all the rules
// around SemVer, which this repo cannot abide by as it uses CalVer.

module github.com/docker/docker

go 1.26.0

require (
	cloud.google.com/go/compute/metadata v0.9.0
	cloud.google.com/go/logging v1.13.1
	code.cloudfoundry.org/clock v1.1.0
	dario.cat/mergo v1.0.0
	github.com/AdaLogics/go-fuzz-headers v0.0.0-20240806141605-e8a1dd7889d6
	github.com/Azure/go-ansiterm v0.0.0-20210617225240-d185dfc1b5a1
	github.com/Graylog2/go-gelf v0.0.0-20191017102106-1550ee647df0
	github.com/Microsoft/go-winio v0.6.2
	github.com/Microsoft/hcsshim v0.11.7
	github.com/RackSec/srslog v0.0.0-20180709174129-a4725f04ec91
	github.com/aws/aws-sdk-go-v2 v1.36.3
	github.com/aws/aws-sdk-go-v2/config v1.29.12
	github.com/aws/aws-sdk-go-v2/credentials v1.17.65
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.30
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.32.0
	github.com/aws/smithy-go v1.22.3
	github.com/cloudflare/cfssl v1.6.4
	github.com/containerd/cgroups/v3 v3.0.3
	github.com/containerd/containerd v1.7.33
	github.com/containerd/containerd/api v1.8.0
	github.com/containerd/continuity v0.4.4
	github.com/containerd/errdefs v1.0.0
	github.com/containerd/fifo v1.1.0
	github.com/containerd/log v0.1.0
	github.com/containerd/platforms v0.2.1
	github.com/containerd/typeurl/v2 v2.1.1
	github.com/coreos/go-systemd/v22 v22.6.0
	github.com/cpuguy83/tar2go v0.3.1
	github.com/creack/pty v1.1.18
	github.com/deckarep/golang-set/v2 v2.3.0
	github.com/distribution/reference v0.6.0
	github.com/docker/distribution v2.8.3+incompatible
	github.com/docker/go-connections v0.5.0
	github.com/docker/go-events v0.0.0-20190806004212-e31b211e4f1c
	github.com/docker/go-metrics v0.0.1
	github.com/docker/go-units v0.5.0
	github.com/docker/libtrust v0.0.0-20150526203908-9cbd2a1374f4
	github.com/fluent/fluent-logger-golang v1.9.0
	github.com/godbus/dbus/v5 v5.1.0
	github.com/gogo/protobuf v1.3.2
	github.com/golang/gddo v0.0.0-20190904175337-72a348e765d2
	github.com/golang/protobuf v1.5.4
	github.com/google/go-cmp v0.7.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.1
	github.com/hashicorp/go-immutable-radix v1.3.1
	github.com/hashicorp/go-memdb v1.3.2
	github.com/hashicorp/go-multierror v1.1.1
	github.com/hashicorp/memberlist v0.4.0
	github.com/hashicorp/serf v0.8.5
	github.com/ishidawataru/sctp v0.0.0-20230406120618-7ff4192f6ff2
	github.com/klauspost/compress v1.18.7
	github.com/miekg/dns v1.1.43
	github.com/mistifyio/go-zfs/v3 v3.0.1
	github.com/mitchellh/copystructure v1.2.0
	github.com/moby/buildkit v0.12.5 // FIXME: revert on buildkit bump https://github.com/moby/moby/commit/7d80641
	github.com/moby/ipvs v1.1.0
	github.com/moby/locker v1.0.1
	github.com/moby/patternmatcher v0.6.1
	github.com/moby/pubsub v1.0.0
	github.com/moby/swarmkit/v2 v2.1.1
	github.com/moby/sys/mount v0.3.5
	github.com/moby/sys/mountinfo v0.7.2
	github.com/moby/sys/sequential v0.7.0
	github.com/moby/sys/signal v0.7.0
	github.com/moby/sys/symlink v0.2.0
	github.com/moby/sys/user v0.4.1
	github.com/moby/sys/userns v0.1.0
	github.com/moby/term v0.5.0
	github.com/montanaflynn/stats v0.7.1
	github.com/morikuni/aec v1.0.0
	github.com/opencontainers/cgroups v0.0.7
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.0
	github.com/opencontainers/runtime-spec v1.1.0
	github.com/opencontainers/selinux v1.13.1
	github.com/pelletier/go-toml v1.9.5
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.21.1
	github.com/rootless-containers/rootlesskit/v2 v2.0.0
	github.com/sirupsen/logrus v1.9.4
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/tonistiigi/go-archvariant v1.0.0
	github.com/vbatts/tar-split v0.11.5
	github.com/vishvananda/netlink v1.2.1-beta.2
	github.com/vishvananda/netns v0.0.4
	go.etcd.io/bbolt v1.3.10
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.61.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.61.0
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.43.0
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0
	go.opentelemetry.io/otel/trace v1.44.0
	golang.org/x/mod v0.40.0
	golang.org/x/net v0.57.0
	golang.org/x/sync v0.22.0
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.41.0
	golang.org/x/time v0.14.0
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa
	google.golang.org/grpc v1.83.1
	google.golang.org/protobuf v1.36.11
	gotest.tools/v3 v3.5.2
	pgregory.net/rapid v1.2.0
	resenje.org/singleflight v0.4.1
	tags.cncf.io/container-device-interface v0.8.1
)

require (
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.18.2 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.2.8 // indirect
	cloud.google.com/go/longrunning v0.8.0 // indirect
	github.com/AdamKorcz/go-118-fuzz-build v0.0.0-20230306123547-8075edf89bb0 // indirect
	github.com/agext/levenshtein v1.2.3 // indirect
	github.com/anchore/go-struct-converter v0.0.0-20221118182256-c68fdcfa2092 // indirect
	github.com/armon/circbuf v0.0.0-20190214190532-5111143e8da2 // indirect
	github.com/armon/go-metrics v0.4.1 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.34 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.34 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.12.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.12.15 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.25.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.30.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.33.17 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bits-and-blooms/bitset v1.13.0 // indirect
	github.com/cenkalti/backoff/v4 v4.2.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cilium/ebpf v0.17.3 // indirect
	github.com/container-storage-interface/spec v1.5.0 // indirect
	github.com/containerd/cgroups v1.1.0 // indirect
	github.com/containerd/console v1.0.3 // indirect
	github.com/containerd/go-cni v1.1.9 // indirect
	github.com/containerd/go-runc v1.1.0 // indirect
	github.com/containerd/nydus-snapshotter v0.8.2 // indirect
	github.com/containerd/stargz-snapshotter/estargz v0.14.3 // indirect
	github.com/containerd/ttrpc v1.2.7 // indirect
	github.com/containernetworking/cni v1.1.2 // indirect
	github.com/cyphar/filepath-securejoin v0.5.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dimchansky/utfbom v1.1.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/fernet/fernet-go v0.0.0-20211208181803-9f70042a33ee // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/gofrs/flock v0.8.1 // indirect
	github.com/gogo/googleapis v1.4.1 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/certificate-transparency-go v1.1.4 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.11 // indirect
	github.com/googleapis/gax-go/v2 v2.17.0 // indirect
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-msgpack v0.5.5 // indirect
	github.com/hashicorp/go-sockaddr v1.0.2 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/in-toto/attestation v1.1.2 // indirect
	github.com/in-toto/in-toto-golang v0.11.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jmoiron/sqlx v1.3.3 // indirect
	github.com/mitchellh/hashstructure/v2 v2.0.2 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/moby/go-archive v0.3.3 // indirect
	github.com/moby/sys/reexec v0.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/runtime-tools v0.9.1-0.20221107090550-2e043c6bd626 // indirect
	github.com/package-url/packageurl-go v0.1.1-0.20220428063043-89078438f170 // indirect
	github.com/philhofer/fwd v1.1.2 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.63.0 // indirect
	github.com/prometheus/procfs v0.16.0 // indirect
	github.com/sean-/seed v0.0.0-20170313163322-e2103e2c3529 // indirect
	github.com/secure-systems-lab/go-securesystemslib v0.10.0 // indirect
	github.com/shibumi/go-pathspec v1.3.0 // indirect
	github.com/spdx/tools-golang v0.5.1 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/syndtr/gocapability v0.0.0-20200815063812-42c35b437635 // indirect
	github.com/tinylib/msgp v1.1.8 // indirect
	github.com/tonistiigi/fsutil v0.0.0-20230629203738-36ef4d8c0dbb // indirect
	github.com/tonistiigi/go-actions-cache v0.0.0-20220404170428-0bdeb6e1eac7 // indirect
	github.com/tonistiigi/units v0.0.0-20180711220420-6950e57a87ea // indirect
	github.com/tonistiigi/vt100 v0.0.0-20230623042737-f9a4f7ef6531 // indirect
	github.com/weppos/publicsuffix-go v0.15.1-0.20210511084619-b1f36a2d6c0b // indirect
	github.com/zmap/zcrypto v0.0.0-20210511125630-18f1e0152cfc // indirect
	github.com/zmap/zlint/v3 v3.1.0 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.6 // indirect
	go.etcd.io/etcd/pkg/v3 v3.5.6 // indirect
	go.etcd.io/etcd/raft/v3 v3.5.6 // indirect
	go.etcd.io/etcd/server/v3 v3.5.6 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/httptrace/otelhttptrace v0.46.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.43.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.21.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/atomic v1.9.0 // indirect
	go.uber.org/multierr v1.8.0 // indirect
	go.uber.org/zap v1.21.0 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	google.golang.org/api v0.264.0 // indirect
	google.golang.org/genproto v0.0.0-20260128011058-8636f8732409 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/klog/v2 v2.90.1 // indirect
	sigs.k8s.io/yaml v1.3.0 // indirect
	tags.cncf.io/container-device-interface/specs-go v0.8.0 // indirect
)

replace (
	github.com/distribution/reference v0.6.0 => github.com/distribution/reference v0.5.0
	github.com/mitchellh/osext => github.com/kardianos/osext v0.0.0-20190222173326-2bc1f35cddc0
	github.com/moby/buildkit => github.com/Mirantis/buildkit v0.12.6-m.4
)
