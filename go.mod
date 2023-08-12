module github.com/kralicky/protols

go 1.20

require (
	github.com/bmatcuk/doublestar v1.3.4
	github.com/bufbuild/protocompile v0.4.0
	github.com/jhump/protoreflect v1.15.1
	github.com/kralicky/gpkg v0.0.0-20220311205216-0d8ea9557555
	github.com/samber/lo v1.38.1
	github.com/spf13/cobra v1.7.0
	go.uber.org/multierr v1.11.0
	go.uber.org/zap v1.24.0
	golang.org/x/exp v0.0.0-20230522175609-2e198f4a06a1
	golang.org/x/mod v0.12.0
	golang.org/x/sync v0.3.0
	golang.org/x/tools v0.6.0
	golang.org/x/tools/gopls v0.12.4
	google.golang.org/genproto/googleapis/api v0.0.0-20230711160842-782d3b101e98
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230706204954-ccb25ca9f130
	google.golang.org/protobuf v1.31.0
)

require (
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/plar/go-adaptive-radix-tree v1.0.5 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.11.0 // indirect
	golang.org/x/telemetry v0.0.0-20230808152233-a65b40c0fdb0 // indirect
	golang.org/x/text v0.12.0 // indirect
	golang.org/x/vuln v0.0.0-20230110180137-6ad3e3d07815 // indirect
	google.golang.org/genproto v0.0.0-20230706204954-ccb25ca9f130 // indirect
)

replace (
	github.com/bufbuild/protocompile => ./protocompile
	github.com/jhump/protoreflect => ./protoreflect
	golang.org/x/tools => ./tools
	golang.org/x/tools/gopls => ./tools/gopls
)
