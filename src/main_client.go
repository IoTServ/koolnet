package main

import (
	//"runtime"
	//"github.com/davecheney/profile"

	"./client"
)

func main() {
	//runtime.GOMAXPROCS(runtime.NumCPU())
	//defer profile.Start(profile.CPUProfile).Stop()
	//defer profile.Start(profile.MemProfile).Stop()
	client.ClientMain()
}
