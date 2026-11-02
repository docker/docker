module github.com/moby/moby/v2

go 1.26.8

tool (
	github.com/gogo/protobuf/protoc-gen-gogo
	github.com/gogo/protobuf/protoc-gen-gogofaster
	github.com/gogo/protobuf/protoc-gen-gogoslick
	github.com/moby/extensions/cmd/mobyextgen
	google.golang.org/protobuf/cmd/protoc-gen-go
)

require (
	cloud.google.com/go/compute/metadata v0.10.0
	cloud.google.com/go/logging v1.21.0
	dario.cat/mergo v1.0.2
	github.com/Azure/go-ansiterm v0.0.0-20261013165250-51075c6ea6bc
	github.com/Graylog2/go-gelf v0.0.0-20191017102106-1550ee647df0 // head of v2 branch
	github.com/Microsoft/go-winio v0.6.4
	github.com/Microsoft/hcsshim v0.15.0
	github.com/RackSec/srslog v0.0.0-20180709174129-a4725f04ec91
	github.com/aws/aws-sdk-go-v2 v1.47.5
	github.com/aws/aws-sdk-go-v2/config v1.33.12
	github.com/aws/aws-sdk-go-v2/credentials v1.20.12
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.5
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.90.3
	github.com/aws/smithy-go v1.28.7
	github.com/cloudflare/cfssl v1.6.5
	github.com/containerd/cgroups/v3 v3.1.3
	github.com/containerd/containerd/api v1.12.0
	github.com/containerd/containerd/v2 v2.4.2
	github.com/containerd/continuity v0.5.0
	github.com/containerd/errdefs v1.0.0
	github.com/containerd/errdefs/pkg v0.3.0
	github.com/containerd/fifo v1.1.0
	github.com/containerd/log v0.2.0
	github.com/containerd/log/otel v0.1.0
	github.com/containerd/nri v0.12.5
	github.com/containerd/platforms v1.0.0-rc.5
	github.com/containerd/plugin v1.1.0
	github.com/containerd/ttrpc v1.2.10
	github.com/containerd/typeurl/v2 v2.3.0
	github.com/coreos/go-systemd/v22 v22.7.0
	github.com/cpuguy83/tar2go v0.4.0
	github.com/creack/pty v1.1.24
	github.com/distribution/reference v0.6.0
	github.com/docker/distribution v2.8.3+incompatible
	github.com/docker/go-connections v0.8.2
	github.com/docker/go-events v0.1.0
	github.com/docker/go-metrics v0.1.0
	github.com/docker/go-units v0.5.0
	github.com/fluent/fluent-logger-golang v1.10.2
	github.com/godbus/dbus/v5 v5.2.4
	github.com/gogo/protobuf v1.3.2
	github.com/google/go-cmp v0.7.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.1
	github.com/hashicorp/go-immutable-radix/v2 v2.1.0
	github.com/hashicorp/go-memdb v1.3.5
	github.com/hashicorp/memberlist v0.7.0
	github.com/in-toto/in-toto-golang v0.11.0
	github.com/ishidawataru/sctp v0.0.0-20251114114122-19ddcbc6aae2
	github.com/mdlayher/socket v0.8.0
	github.com/miekg/dns v1.1.73
	github.com/mistifyio/go-zfs/v4 v4.0.0
	github.com/mitchellh/copystructure v1.2.0
	github.com/moby/buildkit v0.34.1
	github.com/moby/docker-image-spec v1.3.1
	github.com/moby/extensions v0.0.0-20261020100352-d2819d88bcbe
	github.com/moby/go-archive v0.3.3
	github.com/moby/ipvs v1.1.0
	github.com/moby/locker v1.0.1
	github.com/moby/moby/api v1.56.1
	github.com/moby/moby/client v0.6.2
	github.com/moby/patternmatcher v0.6.1
	github.com/moby/policy-helpers v0.0.0-20261022152518-485a1bec29f6
	github.com/moby/profiles/apparmor v0.2.2
	github.com/moby/profiles/seccomp v0.2.4
	github.com/moby/pubsub v1.0.0
	github.com/moby/swarmkit/v2 v2.1.3-0.20261016144904-5d8dbdc21997
	github.com/moby/sys/atomicwriter v0.1.0
	github.com/moby/sys/mount v0.3.5
	github.com/moby/sys/mountinfo v0.7.3
	github.com/moby/sys/reexec v0.1.0
	github.com/moby/sys/signal v0.7.1
	github.com/moby/sys/symlink v0.3.0
	github.com/moby/sys/user v0.4.1
	github.com/moby/sys/userns v0.2.1
	github.com/moby/term v0.5.2
	github.com/opencontainers/cgroups v0.1.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
	github.com/opencontainers/runtime-spec v1.3.0
	github.com/opencontainers/selinux v1.15.1
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.25.0
	github.com/rootless-containers/rootlesskit/v3 v3.3.0
	github.com/sirupsen/logrus v1.10.2
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/tonistiigi/fsutil v0.0.0-20261027232937-ca65610c2295
	github.com/tonistiigi/go-archvariant v1.0.0
	github.com/vbatts/tar-split v0.12.3
	github.com/vishvananda/netlink v1.3.1
	github.com/vishvananda/netns v0.0.5
	go.etcd.io/bbolt v1.5.0
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.72.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0
	go.opentelemetry.io/contrib/processors/baggagecopy v0.17.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	golang.org/x/net v0.62.0
	golang.org/x/sync v0.24.0
	golang.org/x/sys v0.49.0
	golang.org/x/text v0.43.0
	golang.org/x/time v0.16.0
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	gotest.tools/v3 v3.5.2
	pgregory.net/rapid v1.3.0
	resenje.org/singleflight v0.4.3
	tags.cncf.io/container-device-interface v1.1.1
)

require (
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.20.0 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.2.8 // indirect
	cloud.google.com/go/longrunning v1.3.0 // indirect
	code.cloudfoundry.org/clock v1.60.0 // indirect
	cyphar.com/go-pathrs v0.2.5 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.21.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/storage/azblob v1.5.0 // indirect
	github.com/ProtonMail/go-crypto v1.4.1 // indirect
	github.com/agext/levenshtein v1.2.3 // indirect
	github.com/anchore/go-struct-converter v0.1.0 // indirect
	github.com/armon/circbuf v0.0.0-20190214190532-5111143e8da2 // indirect
	github.com/asaskevich/govalidator v0.0.0-20230301143203-a9d515a09cc2 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.25 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.12.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.6 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bits-and-blooms/bitset v1.25.0 // indirect
	github.com/blang/semver v3.5.1+incompatible // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cilium/ebpf v0.22.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/container-storage-interface/spec v1.10.0 // indirect
	github.com/containerd/accelerated-container-image v1.3.0 // indirect
	github.com/containerd/console v1.0.5 // indirect
	github.com/containerd/go-cni v1.1.14 // indirect
	github.com/containerd/go-runc v1.2.1 // indirect
	github.com/containerd/nydus-snapshotter v0.15.15 // indirect
	github.com/containerd/otelttrpc v0.1.0 // indirect
	github.com/containerd/stargz-snapshotter/estargz v0.18.2 // indirect
	github.com/containernetworking/cni v1.3.1 // indirect
	github.com/containernetworking/plugins v1.9.1 // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/cyberphone/json-canonicalization v0.0.0-20241213102144-19d51d7fe467 // indirect
	github.com/cyphar/filepath-securejoin v0.7.0 // indirect
	github.com/digitorus/pkcs7 v0.0.0-20230818184609-3a137a874352 // indirect
	github.com/digitorus/timestamp v0.0.0-20231217203849-220c5c2851b7 // indirect
	github.com/dimchansky/utfbom v1.1.1 // indirect
	github.com/docker/libtrust v0.0.0-20150526203908-9cbd2a1374f4 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/fernet/fernet-go v0.0.0-20240119011108-303da6aec611 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/fxamacker/cbor/v2 v2.9.4 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/analysis v1.0.0 // indirect
	github.com/go-openapi/errors v0.22.9 // indirect
	github.com/go-openapi/jsonpointer v1.0.1 // indirect
	github.com/go-openapi/jsonreference v1.0.1 // indirect
	github.com/go-openapi/loads v0.25.2 // indirect
	github.com/go-openapi/runtime v0.33.2 // indirect
	github.com/go-openapi/runtime/server-middleware v0.33.2 // indirect
	github.com/go-openapi/spec v1.0.0 // indirect
	github.com/go-openapi/strfmt v0.27.2 // indirect
	github.com/go-openapi/swag v0.29.2 // indirect
	github.com/go-openapi/swag/cmdutils v0.29.2 // indirect
	github.com/go-openapi/swag/conv v0.29.2 // indirect
	github.com/go-openapi/swag/fileutils v0.29.2 // indirect
	github.com/go-openapi/swag/jsonutils v0.29.2 // indirect
	github.com/go-openapi/swag/loading v0.29.2 // indirect
	github.com/go-openapi/swag/mangling v0.29.2 // indirect
	github.com/go-openapi/swag/netutils v0.29.2 // indirect
	github.com/go-openapi/swag/pools v0.29.2 // indirect
	github.com/go-openapi/swag/stringutils v0.29.2 // indirect
	github.com/go-openapi/swag/typeutils v0.29.2 // indirect
	github.com/go-openapi/swag/yamlutils v0.29.2 // indirect
	github.com/go-openapi/validate v1.0.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gofrs/flock v0.13.1 // indirect
	github.com/gohugoio/hashstructure v0.6.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/certificate-transparency-go v1.3.3 // indirect
	github.com/google/go-containerregistry v0.22.0 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.17 // indirect
	github.com/googleapis/gax-go/v2 v2.26.2 // indirect
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.31.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-immutable-radix v1.3.1 // indirect
	github.com/hashicorp/go-metrics v0.7.0 // indirect
	github.com/hashicorp/go-msgpack/v2 v2.1.5 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-sockaddr v1.0.7 // indirect
	github.com/hashicorp/golang-lru v1.0.2 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/hiddeco/sshsig v0.2.0 // indirect
	github.com/in-toto/attestation v1.2.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/intel/goresctrl v0.13.0 // indirect
	github.com/jmoiron/sqlx v1.4.0 // indirect
	github.com/klauspost/compress v1.20.2 // indirect
	github.com/knqyf263/go-plugin v0.9.0 // indirect
	github.com/mdlayher/vsock v1.3.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/moby/sys/sequential v0.7.0 // indirect
	github.com/morikuni/aec v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/package-url/packageurl-go v0.1.7 // indirect
	github.com/pelletier/go-toml v1.9.5 // indirect
	github.com/petermattis/goid v0.0.0-20240813172612-4fcff4a6cae7 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/sasha-s/go-deadlock v0.3.5 // indirect
	github.com/sean-/seed v0.0.0-20170313163322-e2103e2c3529 // indirect
	github.com/secure-systems-lab/go-securesystemslib v0.11.0 // indirect
	github.com/shibumi/go-pathspec v1.3.0 // indirect
	github.com/sigstore/protobuf-specs v0.5.1 // indirect
	github.com/sigstore/rekor v1.5.3 // indirect
	github.com/sigstore/rekor-tiles/v2 v2.3.0 // indirect
	github.com/sigstore/sigstore v1.10.8 // indirect
	github.com/sigstore/sigstore-go v1.2.2 // indirect
	github.com/sigstore/timestamp-authority/v2 v2.1.2 // indirect
	github.com/spdx/tools-golang v0.5.7 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/tetratelabs/wazero v1.11.0 // indirect
	github.com/theupdateframework/go-tuf/v2 v2.4.2 // indirect
	github.com/tinylib/msgp v1.4.0 // indirect
	github.com/tonistiigi/dchapes-mode v0.0.0-20250318174251-73d941a28323 // indirect
	github.com/tonistiigi/go-actions-cache v0.0.0-20260930145529-afe8013b5ac4 // indirect
	github.com/tonistiigi/go-csvvalue v0.0.0-20240814133006-030d3b2625d0 // indirect
	github.com/tonistiigi/units v0.0.0-20180711220420-6950e57a87ea // indirect
	github.com/tonistiigi/vt100 v0.0.0-20240514184818-90bafcd6abab // indirect
	github.com/transparency-dev/formats v0.1.1 // indirect
	github.com/transparency-dev/merkle v0.0.2 // indirect
	github.com/weppos/publicsuffix-go v0.30.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	github.com/zmap/zcrypto v0.0.0-20230310154051-c8b263fd8300 // indirect
	github.com/zmap/zlint/v3 v3.5.0 // indirect
	go.etcd.io/etcd/api/v3 v3.6.15 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.6.15 // indirect
	go.etcd.io/etcd/pkg/v3 v3.6.15 // indirect
	go.etcd.io/etcd/server/v3 v3.6.15 // indirect
	go.etcd.io/raft/v3 v3.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/httptrace/otelhttptrace v0.72.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/log v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.1 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/term v0.47.0 // indirect
	google.golang.org/api v0.288.0 // indirect
	google.golang.org/genproto v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	k8s.io/apimachinery v0.37.0 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730 // indirect
	sigs.k8s.io/yaml v1.6.0 // indirect
	tags.cncf.io/container-device-interface/specs-go v1.1.1 // indirect
)

replace github.com/moby/moby/api => ./api

replace github.com/moby/moby/client => ./client
