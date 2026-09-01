package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sql/*.sql
var files embed.FS

func FS() fs.FS {
	sub, err := fs.Sub(files, "sql")
	if err != nil {
		panic(err)
	}
	return sub
}
