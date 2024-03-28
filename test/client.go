package main

import (
	"context"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/robot/client"
)

func main() {
	logger := logging.NewDebugLogger("client")
	robot, err := client.New(
		context.Background(),
		"localhost:8080",
		logger,
	)
	if err != nil {
		logger.Fatal(err)
	}

	defer robot.Close(context.Background())

	logger.Info("Resources:")
	logger.Info(robot.ResourceNames())

	ipCam, err := camera.FromRobot(robot, "ip-cam")
	if err != nil {
		logger.Fatal(err)
	}
	stream, err := ipCam.Stream(context.Background())
	if err != nil {
		logger.Fatal(err)
	}
	_, _, err = stream.Next(context.Background())
	if err != nil {
		logger.Fatal(err)
	}

	logger.Info("All tests passed! Success :)")
}
