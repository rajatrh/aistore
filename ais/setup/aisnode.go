// This file is used to start the AIS daemon
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package main

import (
	"github.com/NVIDIA/aistore/ais"
)

// NOTE: these variables are set by ldflags
var (
	version string
	build   string
)

func main() {
	ais.Run(version, build)
}
