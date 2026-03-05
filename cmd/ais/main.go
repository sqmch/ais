package main

import (
	"os"

	"github.com/sqmch/ais/internal/ais"
)

func main() {
	code := ais.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(code)
}
