package mailtest

import (
	"encoding/base64"
	"io"
	"strings"
)

func newBase64(s string) io.Reader {
	return base64.NewDecoder(base64.StdEncoding, strings.NewReader(strings.TrimSpace(s)))
}
