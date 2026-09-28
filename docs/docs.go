// Package docs embeds the HTTP contract so the running service can serve it.
package docs

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
