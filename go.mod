module github.com/kuasar-sandbox/guest-runtime

go 1.26.1

require (
	github.com/kuasar-sandbox/accelerator v0.1.7
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/AdaLogics/go-fuzz-headers v0.0.0-20230106234847-43070de90fa1 // indirect
	github.com/apex/log v1.9.0 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cyphar/filepath-securejoin v0.5.0 // indirect
	github.com/klauspost/pgzip v1.2.6 // indirect
	github.com/kuasar-sandbox/go-erofs v0.0.0-20261014160540-827da6f90114 // indirect
	github.com/moby/sys/user v0.4.0 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/opencontainers/runtime-spec v1.2.1 // indirect
	github.com/opencontainers/umoci v0.6.0 // indirect
	github.com/rootless-containers/proto/go-proto v0.0.0-20230421021042-4cd87ebadd67 // indirect
	github.com/vbatts/go-mtree v0.6.1-0.20250911112631-8307d76bc1b9 // indirect
)

require (
	github.com/containerd/stargz-snapshotter/estargz v0.16.3 // indirect
	github.com/docker/cli v28.2.2+incompatible // indirect
	github.com/docker/distribution v2.8.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/golang/snappy v1.0.0 // indirect
	github.com/google/go-containerregistry v0.20.6 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/vbatts/tar-split v0.12.1 // indirect
	// Keep this transitive version floor; go mod tidy drops it without direct imports.
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/kuasar-sandbox/accelerator => ../accelerator
