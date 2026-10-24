//go:build !unix

package main

import (
	"errors"
	"os"
)

// Fail closed where Unix root ownership and private modes cannot be checked.
func privateKeyFile(os.FileInfo) error {
	return errors.New("root-private control key validation requires a Unix operating system")
}
