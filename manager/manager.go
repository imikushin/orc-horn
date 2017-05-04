package manager

import (
	"strconv"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"

	"github.com/rancher/longhorn-manager/types"
	"github.com/rancher/longhorn-manager/util"
)

var (
	KeepBadReplicasPeriod = time.Hour * 2
)

type volumeManager struct {
	sync.Mutex

	monitors       map[string]types.Monitor
	addingReplicas map[string]int

	orc     types.Orchestrator
	monitor types.BeginMonitoring

	getController types.GetController
	getBackups    types.GetManagerBackupOps

	settings types.Settings
}

func (man *volumeManager) GetControllerName(volumeName string) string {
	return volumeName + "-controller"
}

func (man *volumeManager) GetReplicaName(volumeName string) string {
	return volumeName + "-replica-" + util.RandomID()
}

func New(orc types.Orchestrator, monitor types.BeginMonitoring, getController types.GetController, getBackups types.GetManagerBackupOps) types.VolumeManager {
	return &volumeManager{
		monitors:       map[string]types.Monitor{},
		addingReplicas: map[string]int{},

		orc:     orc,
		monitor: monitor,

		getController: getController,
		getBackups:    getBackups,

		settings: orc,
	}
}

func (man *volumeManager) doCreate(volume *types.VolumeInfo) (*types.VolumeInfo, error) {
	volume.Created = util.Now()
	vol, err := man.orc.CreateVolume(volume)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create volume '%s'", volume.Name)
	}

	for i := 0; i < vol.NumberOfReplicas; i++ {
		replicaName := man.GetReplicaName(vol.Name)
		if _, err := man.orc.CreateReplica(vol.Name, replicaName); err != nil {
			return nil, errors.Wrapf(err, "error creating replica '%s', volume '%s'", replicaName, vol.Name)
		}
	}
	return man.Get(volume.Name)
}

func (man *volumeManager) cleanupFailedCreate(vol *types.VolumeInfo) {
	if err := man.Delete(vol.Name); err != nil {
		logrus.Warnf("%+v", errors.Wrapf(err, "error deleting volume (failed create) '%s'", vol.Name))
	} else {
		logrus.Debugf("cleaned up after failing to create volume '%s'", vol.Name)
	}
}

func (man *volumeManager) createFromBackup(volume *types.VolumeInfo, backup *types.BackupInfo) (*types.VolumeInfo, error) {
	size, err := strconv.ParseInt(backup.VolumeSize, 10, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing backup.VolumeSize, backup: %+v", backup)
	}
	volume.Size = size
	vol, err := man.doCreate(volume)
	if err != nil {
		return nil, err
	}
	if err := man.doAttach(vol); err != nil {
		defer man.cleanupFailedCreate(vol)
		return nil, errors.Wrapf(err, "failed to attach to restore the backup, volume '%s', backup '%+v'", vol.Name, backup)
	}
	if err := man.getController(vol).BackupOps().Restore(backup.URL); err != nil {
		defer man.cleanupFailedCreate(vol)
		return nil, errors.Wrapf(err, "failed to restore the backup, volume '%s', backup '%+v'", vol.Name, backup)
	}
	if err := man.doDetach(vol); err != nil {
		defer man.cleanupFailedCreate(vol)
		return nil, errors.Wrapf(err, "failed to detach after restoring the backup, volume '%s', backup '%+v'", vol.Name, backup)
	}
	return vol, nil
}

func (man *volumeManager) Create(volume *types.VolumeInfo) (*types.VolumeInfo, error) {
	vol, err := man.Get(volume.Name)
	if err != nil {
		return nil, err
	}
	if vol != nil {
		return nil, errors.Errorf("volume %v already exists", volume.Name)
	}
	settings, err := man.settings.GetSettings()
	if err != nil || settings == nil {
		return nil, errors.New("create volume fail: fail to load settings")
	}
	if volume.EngineImage == "" {
		volume.EngineImage = settings.EngineImage
		if volume.EngineImage == "" {
			return nil, errors.New("create volume fail: No EngineImage specified")
		}
	}
	if volume.FromBackup != "" {
		backupTarget := settings.BackupTarget
		if backupTarget == "" {
			return nil, errors.New("create volume fail: No BackupTarget specified")
		}

		backup, err := man.getBackups(backupTarget).Get(volume.FromBackup)
		if err != nil {
			return nil, errors.Wrapf(err, "error getting backup (to create volume) '%s'", volume.FromBackup)
		}
		return man.createFromBackup(volume, backup)
	}
	return man.doCreate(volume)
}

func (man *volumeManager) Delete(name string) error {
	volume, err := man.Get(name)
	if err != nil {
		return err
	}
	if volume == nil {
		logrus.Warnf("volume %v no longer exist for delete", name)
		return nil
	}

	if err := man.doDetach(volume); err != nil {
		return errors.Wrapf(err, "error detaching for delete, volume '%s'", volume.Name)
	}

	for _, replica := range volume.Replicas {
		if _, err := man.orc.RemoveInstance(&replica.InstanceInfo); err != nil {
			return errors.Wrapf(err, "error removing replica container %s(%s), volume '%s'", replica.Name, replica.ID, volume.Name)
		}
	}

	return errors.Wrapf(man.orc.DeleteVolume(name), "failed to delete volume '%s'", name)
}

func volumeState(volume *types.VolumeInfo) types.VolumeState {
	goodReplicaCount := 0
	for _, replica := range volume.Replicas {
		if replica.BadTimestamp == "" {
			goodReplicaCount++
		}
	}
	switch {
	case goodReplicaCount == 0:
		return types.VolumeStateFaulted
	case volume.Controller == nil:
		return types.VolumeStateDetached
	case goodReplicaCount == volume.NumberOfReplicas:
		return types.VolumeStateHealthy
	}
	return types.VolumeStateDegraded
}

func (man *volumeManager) completeVolumeState(vol *types.VolumeInfo) *types.VolumeInfo {
	vol.State = volumeState(vol)

	vol.Endpoint = ""
	if vol.Controller != nil && vol.Controller.Running {
		vol.Endpoint = man.getController(vol).Endpoint()
	}
	return vol
}

func (man *volumeManager) Get(name string) (*types.VolumeInfo, error) {
	vol, err := man.orc.GetVolume(name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get volume '%s'", name)
	}
	if vol == nil {
		return nil, nil
	}
	return man.completeVolumeState(vol), nil
}

func (man *volumeManager) List() ([]*types.VolumeInfo, error) {
	volumes, err := man.orc.ListVolumes()
	if err != nil {
		return nil, err
	}
	for i, v := range volumes {
		volumes[i] = man.completeVolumeState(v)
	}
	return volumes, nil
}

func (man *volumeManager) Start() error {
	vs, err := man.List()
	if err != nil {
		return err
	}
	for _, v := range vs {
		if v.Controller != nil && v.Controller.Running && v.Controller.HostID == man.orc.GetCurrentHostID() {
			man.startMonitoring(v)
		}
	}
	return nil
}

func (man *volumeManager) startMonitoring(volume *types.VolumeInfo) {
	man.Lock()
	defer man.Unlock()
	if man.monitors[volume.Name] == nil {
		man.monitors[volume.Name] = man.monitor(volume, man)
	}
}

func (man *volumeManager) updateCron(volume *types.VolumeInfo, jobs []*types.RecurringJob) {
	man.Lock()
	defer man.Unlock()
	monitor := man.monitors[volume.Name]
	if monitor == nil {
		return
	}
	if cronCh := monitor.CronCh(); cronCh != nil {
		cronCh <- CronUpdate(jobs)
		logrus.Infof("updated recurring jobs schedule, volume '%s'", volume.Name)
	}
}

func (man *volumeManager) stopMonitoring(volume *types.VolumeInfo) {
	man.Lock()
	defer man.Unlock()
	if mon := man.monitors[volume.Name]; mon != nil {
		mon.Close()
		delete(man.monitors, volume.Name)
	}
}

func (man *volumeManager) Attach(name string) error {
	volume, err := man.Get(name)
	if err != nil {
		return err
	}
	return man.doAttach(volume)
}

func (man *volumeManager) doAttach(volume *types.VolumeInfo) error {
	if volume.Controller != nil {
		if volume.Controller.Running && volume.Controller.HostID == man.orc.GetCurrentHostID() {
			man.startMonitoring(volume)
			return nil
		}
		if err := man.Detach(volume.Name); err != nil {
			return errors.Wrapf(err, "failed to detach before reattaching volume '%s'", volume.Name)
		}
	}
	replicas := map[string]*types.ReplicaInfo{}
	var recentBadReplica *types.ReplicaInfo
	var recentBadK string
	wg := &sync.WaitGroup{}
	errCh := make(chan error)
	for k, replica := range volume.Replicas {
		if replica.Running {
			wg.Add(1)
			go func(replica *types.ReplicaInfo) {
				defer wg.Done()
				if _, err := man.orc.StopInstance(&replica.InstanceInfo); err != nil {
					errCh <- errors.Wrapf(err, "failed to stop replica '%s' for volume '%s'", replica.Name, volume.Name)
				}
			}(replica)
		}
		if replica.BadTimestamp == "" {
			replicas[k] = replica
		} else {
			replicaBadTime, err := util.ParseTime(replica.BadTimestamp)
			if err != nil {
				logrus.Errorf("%+v", err)
				continue
			}
			recentBadTime, err := util.ParseTime(recentBadReplica.BadTimestamp)
			if err != nil {
				logrus.Errorf("%+v", err)
				continue
			}
			if recentBadReplica == nil || replicaBadTime.After(recentBadTime) {
				recentBadReplica = replica
				recentBadK = k
			}
		}
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()
	errs := Errs{}
	for err := range errCh {
		errs = append(errs, err)
		logrus.Errorf("%+v", err)
	}
	if len(errs) > 0 {
		return errs
	}
	if len(replicas) == 0 && recentBadReplica != nil {
		replicas[recentBadK] = recentBadReplica
	}
	if len(replicas) == 0 {
		return errors.Errorf("no replicas to start the controller for volume '%s'", volume.Name)
	}
	wg = &sync.WaitGroup{}
	errCh = make(chan error)
	for _, replica := range replicas {
		wg.Add(1)
		go func(replica *types.ReplicaInfo) {
			defer wg.Done()
			if _, err := man.orc.StartInstance(&replica.InstanceInfo); err != nil {
				errCh <- errors.Wrapf(err, "failed to start replica '%s' for volume '%s'", replica.Name, volume.Name)
			}
		}(replica)
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()
	errs = Errs{}
	for err := range errCh {
		errs = append(errs, err)
		logrus.Errorf("%+v", err)
	}
	if len(errs) > 0 {
		return errs
	}

	controller, err := man.orc.CreateController(volume.Name, man.GetControllerName(volume.Name), replicas)
	if err != nil {
		return errors.Wrapf(err, "failed to start the controller for volume '%s'", volume.Name)
	}

	volume.Controller = controller
	man.startMonitoring(volume)
	return nil
}

func (man *volumeManager) Detach(name string) error {
	volume, err := man.Get(name)
	if err != nil {
		return err
	}
	if volume == nil {
		logrus.Warnf("volume %v no longer exist for detach", name)
		return nil
	}
	return man.doDetach(volume)
}

func (man *volumeManager) doDetach(volume *types.VolumeInfo) error {
	man.stopMonitoring(volume)
	errCh := make(chan error)
	wg := &sync.WaitGroup{}
	if volume.Controller != nil && volume.Controller.Running {
		if _, err := man.orc.StopInstance(&volume.Controller.InstanceInfo); err != nil {
			return errors.Wrapf(err, "error stopping the controller id='%s', volume '%s'", volume.Controller.ID, volume.Name)
		}
	}
	for _, replica := range volume.Replicas {
		wg.Add(1)
		go func(replica *types.ReplicaInfo) {
			defer wg.Done()
			if _, err := man.orc.StopInstance(&replica.InstanceInfo); err != nil {
				errCh <- errors.Wrapf(err, "failed to stop replica '%s' for volume '%s'", replica.Name, volume.Name)
			}
		}(replica)
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()
	errs := Errs{}
	for err := range errCh {
		errs = append(errs, err)
		logrus.Errorf("%+v", err)
	}
	if len(errs) > 0 {
		return errs
	}
	if volume.Controller != nil {
		if _, err := man.orc.RemoveInstance(&volume.Controller.InstanceInfo); err != nil {
			return errors.Wrapf(err, "error removing the controller id='%s', volume '%s'", volume.Controller.ID, volume.Name)
		}
		volume.Controller = nil
	}
	return nil
}

func (man *volumeManager) createAndAddReplicaToController(volumeName string, ctrl types.Controller) error {
	replica, err := man.orc.CreateReplica(volumeName, man.GetReplicaName(volumeName))
	if err != nil {
		return errors.Wrapf(err, "failed to create a replica for volume '%s'", volumeName)
	}
	instance, err := man.orc.StartInstance(&replica.InstanceInfo)
	if err != nil {
		return errors.Wrapf(err, "failed to start replica %v for volume '%s'", replica.Name, volumeName)
	}
	// Update replica.InstanceInfo to provide address for ctrl.AddReplica() call
	replica.InstanceInfo = *instance
	go func() {
		man.addingReplicasCount(volumeName, 1)
		defer man.addingReplicasCount(volumeName, -1)
		if err := ctrl.AddReplica(replica); err != nil {
			logrus.Errorf("%+v", errors.Wrapf(err, "failed to add replica '%s' to volume '%s'", replica.Name, volumeName))
			if _, err := man.orc.StopInstance(&replica.InstanceInfo); err != nil {
				logrus.Errorf("%+v", errors.Wrapf(err, "failed to stop stale replica '%s' of volume '%s'", replica.Name, volumeName))
			}
			if _, err := man.orc.RemoveInstance(&replica.InstanceInfo); err != nil {
				logrus.Errorf("%+v", errors.Wrapf(err, "failed to remove stale replica '%s' of volume '%s'", replica.Name, volumeName))
			}
		}
	}()
	return nil
}

func (man *volumeManager) addingReplicasCount(name string, add int) int {
	man.Lock()
	defer man.Unlock()
	count := man.addingReplicas[name] + add
	man.addingReplicas[name] = count
	return count
}

func (man *volumeManager) UpdateRecurring(name string, jobs []*types.RecurringJob) error {
	volume, err := man.orc.GetVolume(name)
	if err != nil {
		return errors.Wrapf(err, "unable to get volume '%s'", name)
	}
	volume.RecurringJobs = jobs
	if err := man.orc.UpdateVolume(volume); err != nil {
		return errors.Wrapf(err, "unable to update volume '%s'", name)
	}

	if err := ValidateJobs(jobs); err != nil {
		return err
	}

	man.updateCron(volume, jobs)

	return nil
}

func (man *volumeManager) CheckController(ctrl types.Controller, volume *types.VolumeInfo) error {
	replicas, err := ctrl.GetReplicaStates()
	if err != nil {
		return NewControllerError(err)
	}
	logrus.Debugf("checking '%s', NumberOfReplicas=%v: controller knows %v replicas", volume.Name, volume.NumberOfReplicas, len(volume.Replicas))
	goodReplicas := []*types.ReplicaInfo{}
	woReplicas := []*types.ReplicaInfo{}
	errCh := make(chan error)
	wg := &sync.WaitGroup{}
	for _, replica := range replicas {
		switch replica.Mode {
		case types.ReplicaModeRW:
			goodReplicas = append(goodReplicas, replica)
		case types.ReplicaModeWO:
			woReplicas = append(woReplicas, replica)
		case types.ReplicaModeERR:
			wg.Add(1)
			go func(replica *types.ReplicaInfo) {
				defer wg.Done()
				logrus.Warnf("Marking bad replica '%s'", replica.Address)
				wg.Add(2)
				go func() {
					defer wg.Done()
					err := ctrl.RemoveReplica(replica)
					errCh <- errors.Wrapf(err, "failed to remove ERR replica '%s' from volume '%s'", replica.Address, volume.Name)
				}()
				go func() {
					defer wg.Done()
					err := man.orc.MarkBadReplica(volume.Name, replica)
					errCh <- errors.Wrapf(err, "failed to mark replica '%s' bad for volume '%s'", replica.Address, volume.Name)
				}()
			}(replica)
		}
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()
	errs := Errs{}
	for err := range errCh {
		if err == nil {
			continue
		}
		errs = append(errs, err)
		logrus.Errorf("%+v", err)
	}
	if len(errs) > 0 {
		return errs
	}
	if len(goodReplicas) == 0 {
		logrus.Errorf("volume '%s' has no more good replicas, shutting it down", volume.Name)
		return man.Detach(volume.Name)
	}

	addingReplicas := man.addingReplicasCount(volume.Name, 0)
	logrus.Debugf("'%s' replicas by state: RW=%v, WO=%v, adding=%v", volume.Name, len(goodReplicas), len(woReplicas), addingReplicas)
	if len(goodReplicas) < volume.NumberOfReplicas && len(woReplicas) == 0 && addingReplicas == 0 {
		if err := man.createAndAddReplicaToController(volume.Name, ctrl); err != nil {
			return err
		}
	}
	if len(goodReplicas)+len(woReplicas) > volume.NumberOfReplicas {
		logrus.Warnf("volume '%s' has more replicas than needed: has %v, needs %v", volume.Name, len(goodReplicas), volume.NumberOfReplicas)
	}

	return nil
}

func (man *volumeManager) Cleanup(v *types.VolumeInfo) error {
	volume, err := man.Get(v.Name)
	if err != nil {
		return errors.Wrapf(err, "error getting volume '%s'", v.Name)
	}
	if volume == nil {
		logrus.Warnf("volume %v no longer exists for cleanup", v.Name)
		return nil
	}
	logrus.Infof("running cleanup, volume '%s'", volume.Name)
	now := time.Now().UTC()
	errCh := make(chan error)
	wg := &sync.WaitGroup{}
	for _, replica := range volume.Replicas {
		if replica.BadTimestamp == "" {
			continue
		}
		wg.Add(1)
		go func(replica *types.ReplicaInfo) {
			defer wg.Done()
			if replica.Running {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := man.orc.StopInstance(&replica.InstanceInfo)
					errCh <- errors.Wrapf(err, "error stopping bad replica '%s', volume '%s'", replica.Name, volume.Name)
				}()
			}
			badTime, err := util.ParseTime(replica.BadTimestamp)
			if err != nil {
				errCh <- errors.Wrapf(err, "fail to parse bad timestamp %v", replica.BadTimestamp)
				return
			}
			if badTime.Add(KeepBadReplicasPeriod).Before(now) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := man.orc.RemoveInstance(&replica.InstanceInfo)
					errCh <- errors.Wrapf(err, "error removing old bad replica '%s', volume '%s'", replica.Name, volume.Name)
				}()
			}
		}(replica)
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()
	errs := Errs{}
	for err := range errCh {
		if err == nil {
			continue
		}
		errs = append(errs, err)
		logrus.Errorf("%+v", err)
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func (man *volumeManager) Controller(name string) (types.Controller, error) {
	volume, err := man.Get(name)
	if err != nil {
		return nil, err
	}
	return man.getController(volume), nil
}

func (man *volumeManager) SnapshotOps(name string) (types.SnapshotOps, error) {
	controller, err := man.Controller(name)
	if err != nil {
		return nil, err
	}
	return controller.SnapshotOps(), nil
}

func (man *volumeManager) ListHosts() (map[string]*types.HostInfo, error) {
	return man.orc.ListHosts()
}

func (man *volumeManager) GetHost(id string) (*types.HostInfo, error) {
	return man.orc.GetHost(id)
}

func (man *volumeManager) VolumeBackupOps(name string) (types.VolumeBackupOps, error) {
	controller, err := man.Controller(name)
	if err != nil {
		return nil, err
	}
	return controller.BackupOps(), nil
}

func (man *volumeManager) Settings() types.Settings {
	return man.settings
}

func (man *volumeManager) ManagerBackupOps(backupTarget string) types.ManagerBackupOps {
	return man.getBackups(backupTarget)
}

func (man *volumeManager) ProcessSchedule(spec *types.ScheduleSpec, item *types.ScheduleItem) (*types.InstanceInfo, error) {
	scheduler := man.orc.Scheduler()
	if scheduler == nil {
		return nil, errors.Errorf("No scheduler found for the orchestrator")
	}
	return scheduler.Process(spec, item)
}

func (man *volumeManager) ReplicaRemove(volumeName, replicaName string) error {
	volume, err := man.Get(volumeName)
	if err != nil {
		return errors.Wrapf(err, "fail to remove replica %v of volume %v", replicaName, volumeName)
	}
	replica := volume.Replicas[replicaName]
	if replica == nil {
		return errors.Errorf("cannot find replica %v of volume %v", replicaName, volumeName)
	}
	if replica.Running {
		if _, err := man.orc.StopInstance(&replica.InstanceInfo); err != nil {
			logrus.Warnf("cannot stop replica %v of volume %v, push on", replicaName, volumeName)
		}
	}
	if _, err := man.orc.RemoveInstance(&replica.InstanceInfo); err != nil {
		return errors.Wrapf(err, "fail to remove replica %v of volume %v", replicaName, volumeName)
	}
	return nil
}
