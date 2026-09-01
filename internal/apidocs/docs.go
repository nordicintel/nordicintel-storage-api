package apidocs

import (
	_ "embed"
	"net/http"

	"github.com/swaggest/swgui"
	"github.com/swaggest/swgui/v5emb"
)

//go:embed openapi.json
var specification []byte

func Specification() []byte { return append([]byte(nil), specification...) }

func Handler() http.Handler {
	return v5emb.NewWithConfig(swgui.Config{
		Title:       "NordicIntel Storage API",
		SwaggerJSON: "/openapi.json",
		BasePath:    "/docs/",
		SettingsUI: map[string]string{
			"persistAuthorization": "false",
		},
	})("NordicIntel Storage API", "/openapi.json", "/docs/")
}
