package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/kralicky/protols/pkg/lsp"
	"github.com/kralicky/protols/pkg/sources"
	"github.com/mattn/go-tty"
	"github.com/spf13/cobra"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/testing/protopack"
	"google.golang.org/protobuf/types/dynamicpb"
)

// DecodeCmd represents the decode command
func BuildDecodeCmd() *cobra.Command {
	var output string
	var msgType string
	cmd := &cobra.Command{
		Use:   "decode [--type=pkg.Message]",
		Short: "Decodes a protobuf message from stdin and prints it in text format",
		Long: `
If a message type is given with --type, protols will attempt to look up the message
and use it to provide type information when decoding. If the message could not be
found or if no message name is given, a textual representation of the wire format
will be printed instead.
`[1:],
		RunE: func(cmd *cobra.Command, args []string) error {
			in := cmd.InOrStdin()
			if len(msgType) == 0 {
				text, err := decodeWithNoType(cmd.Context(), in)
				if err != nil {
					return err
				}
				cmd.Println(text)
				return nil
			} else {
				msg, err := decodeWithType(cmd.Context(), in, msgType)
				if err != nil {
					return err
				}
				switch output {
				case "text":
					cmd.Println(prototext.MarshalOptions{
						Multiline:    true,
						Indent:       "  ",
						AllowPartial: true,
						EmitUnknown:  true,
					}.Format(msg))
				case "json":
					cmd.Println(protojson.MarshalOptions{
						Multiline:     true,
						Indent:        "  ",
						AllowPartial:  true,
						UseProtoNames: true,
					}.Format(msg))
				}
				return nil
			}
		},
	}
	cmd.Flags().StringVarP(&msgType, "type", "t", "", "The message type to use when decoding")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "Output format (text|json)")
	return cmd
}

func decodeWithNoType(ctx context.Context, in io.Reader) (string, error) {
	input, err := readMaybeBase64AndTrim(in)
	if err != nil {
		return "", err
	}
	msg := protopack.Message{}
	msg.UnmarshalAbductive(input, nil)
	if len(msg) == 0 {
		return "", fmt.Errorf("no input")
	}
	return strings.ReplaceAll(fmt.Sprintf("%+v\n", msg), "\t", "  "), nil
}

func decodeWithType(ctx context.Context, in io.Reader, msgType string) (proto.Message, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cache := lsp.NewCache(protocol.WorkspaceFolder{
		URI: string(protocol.URIFromPath(cwd)),
	})
	cache.LoadFiles(sources.SearchDirs(cwd))
	allMsgs := cache.AllMessages()
	var exact protoreflect.MessageDescriptor
	var exactNameOnly []protoreflect.MessageDescriptor
	var partialMatch []protoreflect.MessageDescriptor
	for _, d := range allMsgs {
		if string(d.FullName()) == msgType {
			// found it
			exact = d
			break
		} else if string(d.Name()) == msgType {
			// found a name match
			exactNameOnly = append(exactNameOnly, d)
		} else if strings.Contains(strings.ToLower(string(d.FullName())), strings.ToLower(msgType)) {
			// found a partial match
			partialMatch = append(partialMatch, d)
		}
	}
	if exact != nil {
		// found an exact match, use it
		return decodeWithDescriptor(ctx, in, exact)
	}
	if len(exactNameOnly) == 1 {
		// found a single name match, use it
		return decodeWithDescriptor(ctx, in, exactNameOnly[0])
	} else if len(exactNameOnly) > 1 {
		// found multiple name matches, prompt the user to choose one
		return chooseAndDecode(ctx, in, exactNameOnly)
	}
	if len(partialMatch) == 1 {
		// found a single partial match, use it
		return decodeWithDescriptor(ctx, in, partialMatch[0])
	} else if len(partialMatch) > 1 {
		// found multiple partial matches, prompt the user to choose one
		return chooseAndDecode(ctx, in, partialMatch)
	}

	return nil, fmt.Errorf("could not find a matching type for %q", msgType)
}

func chooseAndDecode(ctx context.Context, in io.Reader, choices []protoreflect.MessageDescriptor) (proto.Message, error) {
	var selected string
	tty, err := tty.Open()
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	if err := survey.AskOne(&survey.Select{
		Message: "Multiple message types found, choose one:",
		Options: func() []string {
			var opts []string
			for _, d := range choices {
				opts = append(opts, string(d.FullName()))
			}
			return opts
		}(),
		Default: string(choices[0].FullName()),
	}, &selected, survey.WithStdio(tty.Input(), tty.Output(), tty.Output())); err != nil {
		return nil, err
	}
	for _, d := range choices {
		if string(d.FullName()) == selected {
			return decodeWithDescriptor(ctx, in, d)
		}
	}
	return nil, fmt.Errorf("no type selected")
}

func decodeWithDescriptor(ctx context.Context, in io.Reader, desc protoreflect.MessageDescriptor) (proto.Message, error) {
	input, err := readMaybeBase64AndTrim(in)
	if err != nil {
		return nil, err
	}
	// try to decode as wire format
	newMsg := dynamicpb.NewMessage(desc)
	if err := proto.Unmarshal(input, newMsg); err != nil {
		return nil, fmt.Errorf("could not decode input (wrong type?): %w", err)
	}

	return newMsg, nil
}

func looksLikeBase64(input []byte) bool {
	return len(bytes.Trim(input, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/-_=")) == 0
}

func readMaybeBase64AndTrim(in io.Reader) ([]byte, error) {
	input, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	input = bytes.TrimSpace(input)
	if len(input) == 0 {
		return nil, fmt.Errorf("no input")
	}

	// figure out what kind of input we have
	// 1. check if it's base64 encoded
	if looksLikeBase64(input) {
		encodings := []*base64.Encoding{
			base64.StdEncoding,
			base64.URLEncoding,
			base64.RawStdEncoding,
			base64.RawURLEncoding,
		}
		if bytes.HasSuffix(input, []byte{'='}) {
			encodings = encodings[0:2]
		}
		for _, codec := range encodings {
			decoded, err := codec.DecodeString(string(input))
			if err == nil {
				input = decoded
				break
			}
		}
	}
	return input, nil
}
