package docker

import (
	"flag"
	"os"
	"strconv"
	"testing"

	"github.com/rancher/longhorn-manager/types"

	. "gopkg.in/check.v1"
)

const (
	TestPrefix = "longhorn-manager-test"

	EnvCompTest    = "LONGHORN_MANAGER_TEST_COMP"
	EnvEtcdServer  = "LONGHORN_MANAGER_TEST_ETCD_SERVER"
	EnvEngineImage = "LONGHORN_ENGINE_IMAGE"
)

var (
	quick = flag.Bool("quick", false, "Skip tests require other services")

	VolumeName     = TestPrefix + "-vol"
	ControllerName = VolumeName + "-controller"
	Replica1Name   = VolumeName + "-replica1"
	Replica2Name   = VolumeName + "-replica2"
)

func Test(t *testing.T) { TestingT(t) }

type TestSuite struct {
	d           *dockerOrc
	engineImage string

	// Index by instance.ID
	instanceBin map[string]*types.InstanceInfo
}

var _ = Suite(&TestSuite{})

func (s *TestSuite) SetUpSuite(c *C) {
	compTest := os.Getenv(EnvCompTest)
	if compTest != "true" {
		c.Skip("-quick specified")
	}
}

func (s *TestSuite) SetUpTest(c *C) {
	var err error

	s.instanceBin = make(map[string]*types.InstanceInfo)

	etcdIP := os.Getenv(EnvEtcdServer)
	c.Assert(etcdIP, Not(Equals), "")

	s.engineImage = os.Getenv(EnvEngineImage)
	c.Assert(s.engineImage, Not(Equals), "")

	cfg := &dockerOrcConfig{
		servers: []string{"http://" + etcdIP + ":2379"},
		prefix:  "/longhorn",
	}
	orc, err := newDocker(cfg)
	c.Assert(err, IsNil)
	s.d = orc.(*dockerOrc)
}

func (s *TestSuite) Cleanup() {
	for _, instance := range s.instanceBin {
		s.d.stopInstance(instance)
		s.d.removeInstance(instance)
	}
}

func (s *TestSuite) TestCreateVolume(c *C) {
	var instance *types.InstanceInfo

	defer s.Cleanup()

	volume := &types.VolumeInfo{
		Name:        VolumeName,
		Size:        8 * 1024 * 1024, // 8M
		EngineImage: s.engineImage,
	}
	replica1Data := &dockerScheduleData{
		VolumeName:   volume.Name,
		VolumeSize:   strconv.FormatInt(volume.Size, 10),
		InstanceName: Replica1Name,
		EngineImage:  volume.EngineImage,
	}
	replica1, err := s.d.createReplica(replica1Data)
	c.Assert(err, IsNil)
	c.Assert(replica1.ID, NotNil)
	s.instanceBin[replica1.ID] = replica1

	c.Assert(replica1.HostID, Equals, s.d.GetCurrentHostID())
	c.Assert(replica1.Running, Equals, false)
	c.Assert(replica1.Name, Equals, replica1Data.InstanceName)

	instance, err = s.d.startInstance(replica1)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica1.ID)
	c.Assert(instance.Name, Equals, replica1.Name)
	c.Assert(instance.Running, Equals, true)

	instance, err = s.d.stopInstance(replica1)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica1.ID)
	c.Assert(instance.Name, Equals, replica1.Name)
	c.Assert(instance.Running, Equals, false)

	instance, err = s.d.startInstance(replica1)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica1.ID)
	c.Assert(instance.Name, Equals, replica1.Name)
	c.Assert(instance.Running, Equals, true)

	replica1 = instance

	replica2Data := &dockerScheduleData{
		VolumeName:   volume.Name,
		VolumeSize:   strconv.FormatInt(volume.Size, 10),
		InstanceName: Replica2Name,
		EngineImage:  volume.EngineImage,
	}
	replica2, err := s.d.createReplica(replica2Data)
	c.Assert(err, IsNil)
	c.Assert(replica2.ID, NotNil)
	s.instanceBin[replica2.ID] = replica2

	instance, err = s.d.startInstance(replica2)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica2.ID)
	c.Assert(instance.Name, Equals, replica2.Name)
	c.Assert(instance.Running, Equals, true)

	replica2 = instance

	data := &dockerScheduleData{
		VolumeName:   volume.Name,
		InstanceName: ControllerName,
		EngineImage:  volume.EngineImage,
		ReplicaURLs: []string{
			"tcp://" + replica1.Address + ":9502",
			"tcp://" + replica2.Address + ":9502",
		},
	}
	controller, err := s.d.createController(data)
	c.Assert(err, IsNil)
	c.Assert(controller.ID, NotNil)
	s.instanceBin[controller.ID] = controller

	c.Assert(controller.HostID, Equals, s.d.GetCurrentHostID())
	c.Assert(controller.Running, Equals, true)
	c.Assert(controller.Name, Equals, ControllerName)

	instance, err = s.d.stopInstance(controller)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, controller.ID)
	c.Assert(instance.Name, Equals, controller.Name)
	c.Assert(instance.Running, Equals, false)

	instance, err = s.d.stopInstance(replica1)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica1.ID)
	c.Assert(instance.Name, Equals, replica1.Name)
	c.Assert(instance.Running, Equals, false)

	instance, err = s.d.stopInstance(replica2)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica2.ID)
	c.Assert(instance.Name, Equals, replica2.Name)
	c.Assert(instance.Running, Equals, false)

	instance, err = s.d.removeInstance(controller)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, controller.ID)
	delete(s.instanceBin, controller.ID)

	instance, err = s.d.removeInstance(replica1)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica1.ID)
	delete(s.instanceBin, replica1.ID)

	instance, err = s.d.removeInstance(replica2)
	c.Assert(err, IsNil)
	c.Assert(instance.ID, Equals, replica2.ID)
	delete(s.instanceBin, replica2.ID)
}
