//go:build !linux

package main

// Privilege dropping only applies to the Linux container.
func dropPrivileges(string) error { return nil }
