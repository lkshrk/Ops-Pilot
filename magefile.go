//go:build mage

package main

import (
	"os"
	"os/exec"
)

// Test runs the full Go test suite.
func Test() error {
	return runGo("test", "./...")
}

// Vet runs Go's static analysis.
func Vet() error {
	return runGo("vet", "./...")
}

func runGo(args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
