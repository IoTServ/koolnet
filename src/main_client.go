package main

import (
	"runtime"

	//profile "./profile"

	"./client"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	//defer profile.Start(profile.CPUProfile).Stop()
	//defer profile.Start(profile.MemProfile).Stop()
	client.ClientMain()
}
