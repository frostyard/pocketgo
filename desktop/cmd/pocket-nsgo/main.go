package main

import (
	"github.com/frostyard/pocketgo/desktop/internal/nsgoclient"
	"github.com/nsgo-dev/nsgo"
)

func main() { nsgo.Run(nsgoclient.Root, nsgo.WindowSize(1230, 800)) }
