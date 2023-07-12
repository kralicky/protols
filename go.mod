module github.com/kralicky/protols

go 1.20

require (
	github.com/bmatcuk/doublestar v1.3.4
	github.com/bufbuild/protocompile v0.4.0
	github.com/jhump/protoreflect v1.15.1
	github.com/kralicky/gpkg v0.0.0-20220311205216-0d8ea9557555
	github.com/samber/lo v1.38.1
	github.com/spf13/pflag v1.0.5
	go.uber.org/atomic v1.11.0
	go.uber.org/multierr v1.11.0
	go.uber.org/zap v1.24.0
	golang.org/x/exp v0.0.0-20230522175609-2e198f4a06a1
	golang.org/x/mod v0.12.0
	golang.org/x/sync v0.3.0
	golang.org/x/tools v0.6.0
	golang.org/x/tools/gopls v0.12.4
	google.golang.org/genproto/googleapis/api v0.0.0-20230530153820-e85fd2cbaebc
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230530153820-e85fd2cbaebc
	google.golang.org/protobuf v1.31.0
)

require (
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/plar/go-adaptive-radix-tree v1.0.5 // indirect
	golang.org/x/sys v0.10.0 // indirect
	golang.org/x/text v0.11.0 // indirect
	golang.org/x/vuln v0.0.0-20230110180137-6ad3e3d07815 // indirect
	google.golang.org/genproto v0.0.0-20230525234025-438c736192d0 // indirect
)

replace (
	github.com/bufbuild/protocompile => github.com/kralicky/protocompile v0.0.0-20230711023843-20ebac5ec1ec
	github.com/jhump/protoreflect => github.com/kralicky/protoreflect v0.0.0-20230708190847-6393c39ef464
	golang.org/x/tools => github.com/kralicky/tools v0.0.0-20230709215314-5ff1feb7fc58
	golang.org/x/tools/gopls => github.com/kralicky/tools/gopls v0.0.0-20230709215314-5ff1feb7fc58
)
