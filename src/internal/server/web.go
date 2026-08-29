package server

import (
	"embed"
	"io/fs"
)

//go:embed all:webui
var webFS embed.FS

func webRoot() fs.FS {
	sub, err := fs.Sub(webFS, "webui")
	if err != nil {
		panic(err)
	}
	return sub
}
