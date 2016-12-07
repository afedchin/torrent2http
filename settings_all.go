// +build !arm

package main

import "github.com/scakemyer/libtorrent-go"

// Nothing to do on regular devices
func setPlatformSpecificSettings(settings libtorrent.SessionSettings) {
}
