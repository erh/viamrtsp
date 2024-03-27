package main

import (
	"context"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"

	"github.com/erh/viamrtsp"
	"go.viam.com/utils"
)

func main() {
	utils.ContextualMain(mainWithArgs, module.NewLoggerFromArgs("client"))
}

func mainWithArgs(ctx context.Context, args []string, logger logging.Logger) error {
	myMod, err := module.NewModuleFromArgs(ctx, logger)
	if err != nil {
		return err
	}

	viamrtsp.SetLibAVLogLevelFatal()
	err = myMod.AddModelFromRegistry(ctx, camera.API, viamrtsp.ModelH264)
	if err != nil {
		return err
	}

	err = myMod.Start(ctx)
	defer myMod.Close(ctx)
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
