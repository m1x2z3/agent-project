package llm

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func FuzzSSEEventParser(f *testing.F) {
	f.Add("data: {\"id\":\"chatcmpl-1\",\"choices\":[]}\n\ndata: [DONE]\n\n")
	f.Add(": keep-alive\n\ndata: [DONE]\n\n")
	f.Add("data: {not-json}\n\n")

	f.Fuzz(func(t *testing.T, input string) {
		stream := &chatStream{
			reader: bufio.NewReader(strings.NewReader(input)),
			body:   io.NopCloser(strings.NewReader("")),
		}
		for range 4 {
			_, err := stream.Recv()
			if err != nil {
				return
			}
		}
	})
}
