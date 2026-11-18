//go:build !e2efault

package main

// applyFaults — в обычной сборке внедрения отказов нет; fault_e2e.go собирается только с тегом e2efault.
func applyFaults(*sys) {}
