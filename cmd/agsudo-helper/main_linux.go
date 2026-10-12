//go:build linux

package main

import (
	"github.com/thezzisu/agsudo/internal/helper"
	"os"
)

func main() { os.Exit(helper.Main(os.Args[1:])) }
