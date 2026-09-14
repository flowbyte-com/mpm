package main

import "os"

// osReadFileReal reads the entire file. Used by style_guard_test.go.
func osReadFileReal(p string) ([]byte, error) {
	return os.ReadFile(p)
}
