// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package main

//go:generate go run ./gen/gen.go -- tsprotocol.go protocol/tsprotocol.go
//go:generate go run ./gen/gen.go -- tsdocument_changes.go protocol/tsdocument_changes.go
//go:generate go run ./gen/gen.go -- tsserver.go protocol/tsserver.go
//go:generate go run ./gen/gen.go -- tsclient.go protocol/tsclient.go
//go:generate go run ./gen/gen.go -- protocol.go protocol/protocol.go
