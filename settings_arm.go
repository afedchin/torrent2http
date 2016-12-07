// +build arm

package main

import (
	"runtime"

	lt "github.com/scakemyer/libtorrent-go"
)

const (
	maxSingleCoreConnections = 50
)

// On Raspberry Pi, we need to limit the number of active connections
// because otherwise it fries. So here we need to detect that we are on RPi
// (or, rather, a single cpu arm machine, no need to be specific to RPi) and
// set those limits.
// See https://github.com/steeve/plugin.video.pulsar/issues/24
func setPlatformSpecificSettings(settings lt.SessionSettings) {
	if runtime.NumCPU() == 1 { // single core?
        if limit := settings.GetConnectionsLimit(); limit <= 0 || limit > maxSingleCoreConnections { // current limit is in range
		    settings.SetConnectionsLimit(maxSingleCoreConnections)
        }
	}
}