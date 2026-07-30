package main

import (
	"fmt"
	"os"

	"github.com/charlesfused/s3metrics/internal/buildinfo"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(buildinfo.String())
		return
	}
	fmt.Fprintln(os.Stderr, "s3metrics: not implemented yet")
	os.Exit(1)
}
