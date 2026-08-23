//go:build !linux && !darwin

package cli

func flushInput(int) {}
