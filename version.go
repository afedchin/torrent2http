package main

import (
	"fmt"

	lt "github.com/scakemyer/libtorrent-go"
)

var (
	Version string = "1.1.0" // TODO from git tag
)

func UserAgent() string {
	return fmt.Sprintf("torrent2http/%s libtorrent/%s", Version, lt.LIBTORRENT_VERSION)
}
