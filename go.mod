module github.com/jumpojoy/nico-core-mock

go 1.25.4

require (
	github.com/NVIDIA/infra-controller/rest-api v0.0.0
	github.com/digitalocean/go-libvirt v0.0.0-20260609165003-6254771e63a8
	github.com/gogo/status v1.1.1
	github.com/google/uuid v1.6.0
	github.com/rs/zerolog v1.33.0
	golang.org/x/term v0.40.0
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/gogo/googleapis v0.0.0-20180223154316-0cd9801be74a // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/kdomanski/iso9660 v0.4.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
)

exclude google.golang.org/genproto v0.0.0-20180518175338-11a468237815

replace github.com/NVIDIA/infra-controller/rest-api => ../infra-controller/rest-api
