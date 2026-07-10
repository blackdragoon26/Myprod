package main

import (
	"fmt"
	"os"

	"github.com/blackdragoon26/Myprod/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "poolctl:", err)
		os.Exit(1)
	}
}
