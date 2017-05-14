package main

import (
	"fmt"
	"os"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	"github.com/urfave/cli"

	"github.com/rancher/longhorn-manager/api"
	"github.com/rancher/longhorn-manager/backups"
	"github.com/rancher/longhorn-manager/controller"
	"github.com/rancher/longhorn-manager/eventlog"
	"github.com/rancher/longhorn-manager/manager"
	"github.com/rancher/longhorn-manager/orch"
	"github.com/rancher/longhorn-manager/orch/docker"
	"github.com/rancher/longhorn-manager/types"
	"github.com/rancher/longhorn-manager/util/daemon"
	"github.com/rancher/longhorn-manager/util/server"
)

const (
	sockFile = "/var/run/longhorn/volume-manager.sock"
)

var VERSION = "0.1.0"

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{ForceColors: true})

	app := cli.NewApp()
	app.Version = VERSION
	app.Usage = "Rancher Longhorn storage driver/orchestration"
	app.Action = RunManager

	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:   "debug, d",
			Usage:  "enable debug logging level",
			EnvVar: "RANCHER_DEBUG",
		},
		cli.StringFlag{
			Name:  "orchestrator",
			Usage: "Choose orchestrator: docker",
			Value: "docker",
		},

		cli.StringFlag{
			Name:   orch.EngineImageParam,
			EnvVar: "LONGHORN_ENGINE_IMAGE",
			Usage:  "Specify Longhorn engine image",
		},

		// Docker
		cli.StringSliceFlag{
			Name:  "etcd-servers",
			Usage: "etcd server ip and port, in format `http://etcd1:2379,http://etcd2:2379`",
		},
		cli.StringFlag{
			Name:  "etcd-prefix",
			Usage: "the prefix using with etcd server",
			Value: "/longhorn",
		},
		cli.StringFlag{
			Name:  "docker-network",
			Usage: "use specified docker network, can be omitted for auto detection",
		},
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatalf("Critical error: %v", err)
	}

}

func RunManager(c *cli.Context) error {
	var (
		orc types.Orchestrator
		err error
	)

	if c.Bool("debug") {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if c.String(orch.EngineImageParam) == "" {
		return fmt.Errorf("Must specify %v", orch.EngineImageParam)
	}

	orcName := c.String("orchestrator")
	if orcName == "docker" {
		orc, err = docker.New(c)
	} else {
		err = fmt.Errorf("Invalid orchestrator %v", orcName)
	}
	if err != nil {
		return err
	}

	man := manager.New(orc, manager.Monitor(controller.Get), controller.Get, backups.New)
	si, err := man.Settings().GetSettings()
	if err != nil {
		return err
	}
	if err := eventlog.Update(si.SyslogTarget); err != nil {
		logrus.Warnf("%v", errors.Wrap(err, "unable to set up event log"))
	}
	if err := man.Start(); err != nil {
		return err
	}

	proxy := api.Proxy()

	s := api.NewServer(man, orc, proxy)

	go server.NewUnixServer(sockFile).Serve(api.Handler(s))
	go server.NewTCPServer(fmt.Sprintf(":%v", api.DefaultPort)).Serve(api.Handler(s))

	return daemon.WaitForExit()
}
