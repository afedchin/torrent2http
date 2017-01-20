package main

import (
	"fmt"

	"github.com/scakemyer/libtorrent-go"
)

var (
	Version string = "1.1.3" // TODO from git tag
)

func UserAgent() string {
	return fmt.Sprintf("torrent2http/%s libtorrent/%s", Version, libtorrent.Version())
}
