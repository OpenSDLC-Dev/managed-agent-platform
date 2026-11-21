// Package uidguard defines the admission contract shared by the gate image and
// both sandbox providers. The launcher links only the standard library.
package uidguard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	GateUID  = 65532
	Refused  = 77
	Dir      = "/map-sandbox-guard"
	Path     = Dir + "/uidguard"
	StageEnv = "MAP_STAGE_SANDBOX_GUARD"
)

// Command checks the runtime identity before the first executable from the
// sandbox image. With no arguments the launcher is an admission probe.
func Command(argv []string) []string {
	return append([]string{Path}, argv...)
}

// Stage publishes the trusted binary before the gate reports ready. A gate
// restart must never truncate the executable another container is using.
func Stage(source, dir string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".uidguard-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = io.Copy(tmp, src); err == nil {
		err = tmp.Chmod(0555)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("stage sandbox UID guard: %w", err)
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "uidguard"))
}
