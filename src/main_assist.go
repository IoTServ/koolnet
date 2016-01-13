package main

import (
	"./assist"
	"runtime"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	client.AssistMain()
}
