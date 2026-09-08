// Пакет openapi встраивает версионируемый HTTP-контракт, который публикует API.
package openapi

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
