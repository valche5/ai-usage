//go:build !linux && !darwin

package renew

import (
	"errors"
	"os"
)

// tryFlock keeps the package buildable on unsupported platforms. Automatic
// renewal fails closed there instead of running without cross-process safety.
func tryFlock(*os.File) (bool, error) {
	return false, errors.New("verrou inter-process non pris en charge sur cette plateforme")
}
