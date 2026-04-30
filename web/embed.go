package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist
var dist embed.FS

func DistFS() http.FileSystem {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return http.FS(sub)
}
