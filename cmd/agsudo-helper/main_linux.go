//go:build linux

package main

import (
	"github.com/agyard/agsudo/internal/helper"
	"os"
)

func main() { os.Exit(helper.Main(os.Args[1:])) }
