package lsp

import (
	"fmt"
	"reflect"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func Test_relativeFullName(t *testing.T) {
	tests := []struct {
		target  string
		fromPkg string
		want    string
	}{
		{"foo.bar.baz.A", "foo.bar", "baz.A"},
		{"foo.bar.baz.A", "foo.bar.quux", "baz.A"},
		{"foo.bar.baz.A", "foo", "bar.baz.A"},
		{"foo.bar.baz.A", "foo.bar.baz", "A"},
		{"foo.bar.baz.A", "x.y", "foo.bar.baz.A"},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if got := relativeFullName(protoreflect.FullName(tt.target), protoreflect.FullName(tt.fromPkg)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("relativeFullName(%s, %s) = %v, want %v", tt.target, tt.fromPkg, got, tt.want)
			}
		})
	}
}
