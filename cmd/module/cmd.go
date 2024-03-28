package main

import (
	"context"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"

	"github.com/erh/viamrtsp"
)

func main() {
	err := realMain()
	if err != nil {
		panic(err)
	}
}
func realMain() error {

	ctx := context.Background()
	logger := logging.NewDebugLogger("client")

	myMod, err := module.NewModuleFromArgs(ctx, logger)
	if err != nil {
		return err
	}

	for _, model := range viamrtsp.Models {
		err = myMod.AddModelFromRegistry(ctx, camera.API, model)
		if err != nil {
			return err
		}
	}

	err = myMod.Start(ctx)
	defer myMod.Close(ctx)
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
