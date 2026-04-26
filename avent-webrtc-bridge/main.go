package main

import (
	"fmt"
	"os"

	"avent-webrtc-bridge/cmd"
	"avent-webrtc-bridge/pkg/core"
)

const VERSION = "0.0.6"

func main() {
	core.InitLogger()

	if err := cmd.Execute(VERSION); err != nil {
		fmt.Println("Command execution failed")
		os.Exit(1)
	}
}
