module github.com/kralicky/protols

go 1.21

require (
	github.com/bufbuild/protocompile v0.4.0
	github.com/gogo/protobuf v1.3.2
	github.com/jhump/protoreflect v1.15.1
	github.com/kralicky/gpkg v0.0.0-20220311205216-0d8ea9557555
	github.com/samber/lo v1.38.1
	github.com/spf13/cobra v1.7.0
	go.uber.org/multierr v1.11.0
	golang.org/x/exp v0.0.0-20230905200255-921286631fa9
	golang.org/x/mod v0.13.0
	golang.org/x/sync v0.4.0
	golang.org/x/tools v0.14.0
	golang.org/x/tools/gopls v0.12.4
	google.golang.org/genproto v0.0.0-20230815205213-6bfd019c3878
	google.golang.org/genproto/googleapis/api v0.0.0-20230803162519-f966b187b2e5
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230803162519-f966b187b2e5
	google.golang.org/protobuf v1.31.0
)

require (
	cloud.google.com/go/dlp v1.10.1 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/plar/go-adaptive-radix-tree v1.0.5 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	golang.org/x/net v0.16.0 // indirect
	golang.org/x/sys v0.13.0 // indirect
	golang.org/x/telemetry v0.0.0-20231011160506-788d5629a052 // indirect
	golang.org/x/text v0.13.0 // indirect
	golang.org/x/vuln v1.0.1 // indirect
	google.golang.org/grpc v1.57.0 // indirect
)

replace (
	github.com/bufbuild/protocompile => ./protocompile
	github.com/jhump/protoreflect => ./protoreflect
	golang.org/x/tools => ./tools
	golang.org/x/tools/gopls => ./tools/gopls
)
