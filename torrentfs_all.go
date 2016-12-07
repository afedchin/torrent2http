// +build !darwin,!freebsd,!dragonfly

package main

import "os"

func unlockFile(file *os.File) error {
	return nil
}
