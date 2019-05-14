package main

import (
	"strings"

	"github.com/anuvu/stacker"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var grabCmd = cli.Command{
	Name:   "grab",
	Usage:  "grabs a file from the layer's filesystem",
	Action: doGrab,
	ArgsUsage: `<tag>:<path>

<tag> is the tag in a built stacker image to extract the file from.

<path> is the path to extract (relative to /) in the image's rootfs.`,
}

func doGrab(ctx *cli.Context) error {
	s, err := stacker.NewStorage(config)
	if err != nil {
		return err
	}
	defer s.Detach()

	parts := strings.SplitN(ctx.Args().First(), ":", 2)
	if len(parts) < 2 {
		return errors.Errorf("invalid grab argument: %s", ctx.Args().First())
	}

	err = s.Restore(parts[0], stacker.WorkingContainerName)
	if err != nil {
		return err
	}
	defer s.Delete(stacker.WorkingContainerName)

	return stacker.Grab(config, parts[0], parts[1])
}
