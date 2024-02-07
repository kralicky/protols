// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// contains select definitions from protobuf/internal/genid.
package genid

import "google.golang.org/protobuf/reflect/protoreflect"

// Field numbers for google.protobuf.DescriptorProto.
const (
	DescriptorProto_Name_field_number           protoreflect.FieldNumber = 1
	DescriptorProto_Field_field_number          protoreflect.FieldNumber = 2
	DescriptorProto_Extension_field_number      protoreflect.FieldNumber = 6
	DescriptorProto_NestedType_field_number     protoreflect.FieldNumber = 3
	DescriptorProto_EnumType_field_number       protoreflect.FieldNumber = 4
	DescriptorProto_ExtensionRange_field_number protoreflect.FieldNumber = 5
	DescriptorProto_OneofDecl_field_number      protoreflect.FieldNumber = 8
	DescriptorProto_Options_field_number        protoreflect.FieldNumber = 7
	DescriptorProto_ReservedRange_field_number  protoreflect.FieldNumber = 9
	DescriptorProto_ReservedName_field_number   protoreflect.FieldNumber = 10
)

// Field numbers for google.protobuf.FileDescriptorProto.
const (
	FileDescriptorProto_Name_field_number             protoreflect.FieldNumber = 1
	FileDescriptorProto_Package_field_number          protoreflect.FieldNumber = 2
	FileDescriptorProto_Dependency_field_number       protoreflect.FieldNumber = 3
	FileDescriptorProto_PublicDependency_field_number protoreflect.FieldNumber = 10
	FileDescriptorProto_WeakDependency_field_number   protoreflect.FieldNumber = 11
	FileDescriptorProto_MessageType_field_number      protoreflect.FieldNumber = 4
	FileDescriptorProto_EnumType_field_number         protoreflect.FieldNumber = 5
	FileDescriptorProto_Service_field_number          protoreflect.FieldNumber = 6
	FileDescriptorProto_Extension_field_number        protoreflect.FieldNumber = 7
	FileDescriptorProto_Options_field_number          protoreflect.FieldNumber = 8
	FileDescriptorProto_SourceCodeInfo_field_number   protoreflect.FieldNumber = 9
	FileDescriptorProto_Syntax_field_number           protoreflect.FieldNumber = 12
	FileDescriptorProto_Edition_field_number          protoreflect.FieldNumber = 14
)

// Field numbers for google.protobuf.EnumDescriptorProto.
const (
	EnumDescriptorProto_Name_field_number          protoreflect.FieldNumber = 1
	EnumDescriptorProto_Value_field_number         protoreflect.FieldNumber = 2
	EnumDescriptorProto_Options_field_number       protoreflect.FieldNumber = 3
	EnumDescriptorProto_ReservedRange_field_number protoreflect.FieldNumber = 4
	EnumDescriptorProto_ReservedName_field_number  protoreflect.FieldNumber = 5
)

// Field numbers for google.protobuf.ServiceDescriptorProto.
const (
	ServiceDescriptorProto_Name_field_number    protoreflect.FieldNumber = 1
	ServiceDescriptorProto_Method_field_number  protoreflect.FieldNumber = 2
	ServiceDescriptorProto_Options_field_number protoreflect.FieldNumber = 3
)
