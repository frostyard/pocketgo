//go:build !js

package state

import "os"

type snapshotBacking = os.File
