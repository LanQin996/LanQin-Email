//go:build !unix

package app

func applyMaildirOwnership(path string) error {
	return nil
}
