package toolbox_test

import "encoding/base64"

func base64Decode(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(value)
}
