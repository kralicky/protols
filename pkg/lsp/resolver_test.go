package lsp

import "testing"

func TestFindSuffixMatchedPath(t *testing.T) {
	type args struct {
		target string
		source string
	}
	tests := []struct {
		name  string
		args  args
		want  string
		want1 bool
	}{
		{
			name: "test1",
			args: args{
				target: "store/labelpb/types.proto",
				source: "github.com/thanos-io/thanos/pkg/store/storepb/rpc.proto",
			},
			want:  "github.com/thanos-io/thanos/pkg/store/labelpb/types.proto",
			want1: true,
		},
		{
			name: "test2",
			args: args{
				target: "store/storepb/prompb/types.proto",
				source: "github.com/thanos-io/thanos/pkg/store/storepb/rpc.proto",
			},
			want:  "github.com/thanos-io/thanos/pkg/store/storepb/prompb/types.proto",
			want1: true,
		},
		{
			name: "test2",
			args: args{
				target: "store/types.proto",
				source: "github.com/thanos-io/thanos/pkg/store/storepb/rpc.proto",
			},
			want:  "github.com/thanos-io/thanos/pkg/store/types.proto",
			want1: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := FindSuffixMatchedPath(tt.args.target, tt.args.source)
			if got != tt.want {
				t.Errorf("FindSuffixMatchedPath() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("FindSuffixMatchedPath() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}
