package release_test

import "encoding/base64"

func base64Std(value []byte) string { return base64.StdEncoding.EncodeToString(value) }
